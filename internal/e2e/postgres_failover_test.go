//go:build !windows

package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/db"
	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/scheduler"
	"github.com/ashhart/tf-starlight/internal/testutil"
)

type accountingSnapshot struct{ Requests, Tokens, Used, Reserved, Leases int64 }

const authorityResponseLimit = 10 * time.Second

func failoverOutageRequest(t *testing.T, f *fleet, body string) (*http.Response, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, authorityResponseLimit)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, f.public.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+f.key)
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("bounded outage request failed: %v", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		t.Fatalf("bounded outage response failed: %v", err)
	}
	return res, string(raw)
}

func readAccounting(ctx context.Context, f *fleet) (accountingSnapshot, error) {
	var s accountingSnapshot
	err := f.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM usage_events WHERE outcome='ok'),
		(SELECT coalesce(sum(prompt_tokens+completion_tokens),0) FROM usage_events WHERE outcome='ok'),
		(SELECT coalesce(sum(used),0) FROM counters WHERE key=$1),
		(SELECT coalesce(sum(reserved),0) FROM counters WHERE key=$1),
		(SELECT count(*) FROM leases)`, "tokens:day:user:"+f.user.ID).Scan(&s.Requests, &s.Tokens, &s.Used, &s.Reserved, &s.Leases)
	return s, err
}

func TestPostgresStandbyPromotionPreservesAcknowledgedAccounting(t *testing.T) {
	if os.Getenv("STARLIGHT_POSTGRES_DRILL") != "1" {
		t.Skip("set STARLIGHT_POSTGRES_DRILL=1 to create two isolated Postgres17 clusters")
	}
	pair := newPostgresPair(t)
	t.Setenv(testutil.EnvURL, pair.URL())
	f := startFleet(t)
	limitID := ids.New()
	exec(t, f.pool, "INSERT INTO limits(id,scope_type,scope_id,kind,value) VALUES($1,'user',$2,'tokens_per_day',1000000)", limitID, f.user.ID)
	exec(t, f.pool, "CREATE TABLE failover_assertions (id text PRIMARY KEY)")
	exec(t, f.pool, "INSERT INTO failover_assertions VALUES ('acknowledged')")
	body := `{"model":"qwen3.8-27b","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`
	var expectedRequests, expectedTokens int64
	success := func(code int, body string) {
		t.Helper()
		var response struct {
			Usage struct {
				Prompt     int64 `json:"prompt_tokens"`
				Completion int64 `json:"completion_tokens"`
			}
		}
		if code != 200 || json.Unmarshal([]byte(body), &response) != nil || response.Usage.Prompt+response.Usage.Completion <= 0 {
			t.Fatalf("unsuccessful metered fixture request: status %d", code)
		}
		expectedRequests++
		expectedTokens += response.Usage.Prompt + response.Usage.Completion
	}
	response, reply := f.call(http.MethodPost, "/v1/chat/completions", f.key, body)
	success(response.StatusCode, reply)
	lease, record := holdFailoverReservation(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	before, err := readAccounting(ctx, f)
	cancel()
	if err != nil || before.Requests != expectedRequests || before.Tokens != expectedTokens || before.Used != expectedTokens || before.Reserved != 20 || before.Leases != 1 {
		t.Fatalf("accounting before failover: %+v, %v", before, err)
	}
	lost := time.Now()
	pair.stopPrimary()
	outage := time.Now()
	response, reply = failoverOutageRequest(t, f, body)
	unavailableFor := time.Since(outage)
	if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "1" || !strings.Contains(reply, "temporarily_unavailable") {
		t.Fatalf("lost database authority returned status %d, Retry-After %q", response.StatusCode, response.Header.Get("Retry-After"))
	}
	if unavailableFor > authorityResponseLimit {
		t.Fatalf("lost authority took %v to fail closed", unavailableFor)
	}
	promoting := time.Now()
	pair.promote()
	promotionTime := time.Since(promoting)
	f.pool.Reset()
	f.starlight.Pool.Reset()
	deadline := time.Now().Add(30 * time.Second)
	var recovered accountingSnapshot
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = f.pool.Ping(ctx)
		if err == nil {
			err = f.starlight.Pool.Ping(ctx)
		}
		if err == nil {
			recovered, err = readAccounting(ctx, f)
		}
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool did not reconnect after promotion: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if recovered != before {
		t.Fatalf("acknowledged accounting changed across promotion: before %+v, after %+v", before, recovered)
	}
	var marker bool
	if err := f.pool.QueryRow(f.ctx, "SELECT EXISTS(SELECT 1 FROM failover_assertions WHERE id='acknowledged')").Scan(&marker); err != nil || !marker {
		t.Fatal("acknowledged transaction was lost")
	}
	for range 2 {
		if err := f.starlight.Admitter.Settle(f.ctx, lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record); err != nil {
			t.Fatalf("settling the recovered reservation: %v", err)
		}
	}
	expectedRequests++
	expectedTokens += 7
	response, reply = f.call(http.MethodPost, "/v1/chat/completions", f.key, body)
	success(response.StatusCode, reply)
	recoveredIn := time.Since(lost)
	exec(t, f.pool, "UPDATE limits SET value=0 WHERE id=$1", limitID)
	deadline = time.Now().Add(10 * time.Second)
	for {
		response, reply = f.call(http.MethodPost, "/v1/chat/completions", f.key, body)
		if response.StatusCode == http.StatusTooManyRequests && strings.Contains(reply, "budget_limit") {
			break
		}
		success(response.StatusCode, reply)
		if time.Now().After(deadline) {
			t.Fatal("budget enforcement did not recover after promotion")
		}
		time.Sleep(100 * time.Millisecond)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	after, err := readAccounting(ctx, f)
	cancel()
	if err != nil || after.Requests != expectedRequests || after.Tokens != expectedTokens || after.Used != expectedTokens || after.Reserved != 0 || after.Leases != 0 {
		t.Fatalf("settlements after failover: %+v, %v, expected %d requests/%d tokens", after, err, expectedRequests, expectedTokens)
	}
	var holder string
	var epoch int64
	if err := f.pool.QueryRow(f.ctx, "SELECT holder,epoch FROM controller_leader WHERE id").Scan(&holder, &epoch); err != nil {
		t.Fatal(err)
	}
	exec(t, f.pool, "UPDATE controller_leader SET holder=$1,epoch=epoch+1,expires_at=now()+interval '1 minute' WHERE id", ids.New())
	err = db.Tx(f.ctx, f.pool, func(tx pgx.Tx) error {
		if err := scheduler.Fence(f.ctx, tx, holder, epoch); err != nil {
			return err
		}
		_, err := tx.Exec(f.ctx, "INSERT INTO failover_assertions VALUES ('stale-controller')")
		return err
	})
	if !errors.Is(err, scheduler.ErrFenced) {
		t.Fatalf("stale controller was not fenced: %v", err)
	}
	if err := f.pool.QueryRow(f.ctx, "SELECT EXISTS(SELECT 1 FROM failover_assertions WHERE id='stale-controller')").Scan(&marker); err != nil || marker {
		t.Fatal("stale controller committed a write")
	}
	receipt := map[string]any{"declared_source_sha": os.Getenv("STARLIGHT_TEST_HEAD"), "fixture": "single-site synchronous standby, fail-stop primary, stable endpoint",
		"primary_loss_to_success_seconds": recoveredIn.Seconds(), "standby_promotion_seconds": promotionTime.Seconds(), "authority_failure_seconds": unavailableFor.Seconds(),
		"acknowledged_before": before, "settled_after": after, "stale_controller_fenced": true, "reservation_survived": true, "duplicate_settlement_ignored": true, "production_slo_qualified": false,
		"pool_recovery": "explicit reset and ping of fixture and gateway pools"}
	raw, _ := json.Marshal(receipt)
	t.Logf("db_failover_receipt %s", raw)
}

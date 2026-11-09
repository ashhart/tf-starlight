package accounting_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/accounting"
	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/testutil"
)

const capacityInsert = `WITH source AS (SELECT * FROM terminal_leases WHERE lease_id=$1),
	generated AS (SELECT gen_random_uuid() AS id FROM generate_series(1,$2::int)),
	body AS (SELECT g.id,s.project_id,s.kind,s.charged_tokens,s.charged_micros,s.observed,
		(s.receipt::jsonb||jsonb_build_object('lease',g.id,'ended_at',now()-interval '8 days',
		'late_until',now()-interval '1 day'))::text AS frozen FROM generated g CROSS JOIN source s)
	INSERT INTO terminal_leases(lease_id,project_id,kind,charged_tokens,charged_micros,observed,ended_at,late_until,receipt,receipt_hash)
	SELECT id,project_id,kind,charged_tokens,charged_micros,observed,now()-interval '8 days',now()-interval '1 day',frozen,
	sha256(convert_to(frozen,'UTF8')) FROM body`

func TestAccountingCleanupSustainsTwoHundredExpiredReceiptsASecond(t *testing.T) {
	if os.Getenv("STARLIGHT_ACCOUNTING_CAPACITY") != "1" {
		t.Skip("set STARLIGHT_ACCOUNTING_CAPACITY=1 for the 45-second retention-capacity fixture")
	}
	pool := testutil.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	lease := ids.New()
	_, err := pool.Exec(ctx, `INSERT INTO leases(id,request_id,gateway_id,model_id,replica_id,scopes,priority,deadline)
		VALUES($1,'capacity-fixture',$2,$3,$4,'{}','standard',now());`, lease, ids.New(), ids.New(), ids.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "SELECT starlight_release($1,0,0)", lease); err != nil {
		t.Fatal(err)
	}
	retainedInsert := strings.NewReplacer("now()-interval '8 days'", "now()",
		"now()-interval '1 day'", "now()+interval '7 days'").Replace(capacityInsert)
	if _, err := pool.Exec(ctx, retainedInsert, lease, 50000); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var produced, maxBatch, producerElapsed atomic.Int64
	var producer sync.WaitGroup
	errCh := make(chan error, 1)
	producer.Add(1)
	go func() {
		defer producer.Done()
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			case <-tick.C:
			}
			elapsed := min(time.Since(start), 45*time.Second)
			wanted := int64(elapsed.Seconds() * 200)
			if n := wanted - produced.Load(); n > 0 {
				if n > maxBatch.Load() {
					maxBatch.Store(n)
				}
				if _, err := pool.Exec(ctx, capacityInsert, lease, n); err != nil {
					errCh <- err
					return
				}
				produced.Add(n)
			}
			if elapsed == 45*time.Second {
				producerElapsed.Store(int64(time.Since(start)))
				errCh <- nil
				return
			}
		}
	}()
	defer func() { cancel(); producer.Wait() }()
	var sweeps []map[string]any
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for range 3 {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
		}
		batchCtx, stop := context.WithTimeout(ctx, 2*time.Second)
		at := time.Now()
		r, err := accounting.PruneUntil(batchCtx, pool)
		stop()
		if err != nil || r.BudgetExhausted {
			t.Fatalf("default cleanup budget could not keep up: %+v, %v", r, err)
		}
		var remaining int64
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM terminal_leases WHERE late_until<now()").Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining > 600 {
			t.Fatalf("expired backlog keeps growing at 200/s: %d", remaining)
		}
		sweeps = append(sweeps, map[string]any{"deleted": r.Deleted, "batches": r.Batches,
			"elapsed_seconds": time.Since(at).Seconds(), "remaining_expired": remaining, "produced": produced.Load()})
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	finalCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	final, err := accounting.PruneUntil(finalCtx, pool)
	stop()
	var expired, retained int64
	if queryErr := pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE late_until<now()),
		count(*) FILTER(WHERE late_until>=now()) FROM terminal_leases`).Scan(&expired, &retained); queryErr != nil {
		t.Fatal(queryErr)
	}
	if err != nil || final.BudgetExhausted || produced.Load() != 9000 || expired != 0 || retained != 50001 || maxBatch.Load() > 200 {
		t.Fatalf("capacity fixture incomplete: produced=%d expired=%d retained=%d max_batch=%d final=%+v: %v",
			produced.Load(), expired, retained, maxBatch.Load(), final, err)
	}
	record, _ := json.Marshal(map[string]any{"basis": "local controlled fixture, not seven-day fleet-volume qualification",
		"offered_rate": 200, "offered_seconds": 45, "producer_elapsed_seconds": time.Duration(producerElapsed.Load()).Seconds(),
		"produced": produced.Load(), "max_producer_batch": maxBatch.Load(), "retained_unexpired": retained, "sweeps": sweeps})
	t.Log(string(record))
}

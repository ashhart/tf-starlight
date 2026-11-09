//go:build !windows

package e2e_test

import (
	"context"
	"encoding/json"
	"os"
	osexec "os/exec"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/accounting"
	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/audit"
	"github.com/ashhart/tf-starlight/internal/db"
	"github.com/ashhart/tf-starlight/internal/ids"
)

func TestTerminalAccountingSurvivesDatabaseAndWorkerRestart(t *testing.T) {
	if os.Getenv("STARLIGHT_POSTGRES_DRILL") != "1" {
		t.Skip("set STARLIGHT_POSTGRES_DRILL=1 to create isolated Postgres17 clusters")
	}
	pair := newPostgresPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, pair.URL(), db.Options{MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	lease, gateway, model := ids.New(), ids.New(), ids.New()
	project, principal, replica := ids.New(), ids.New(), ids.New()
	key := "tokens:day:project:" + project
	exec(t, pool, `INSERT INTO counters(key,window_start,reserved) VALUES($1,date_trunc('day',now()),100)`, key)
	exec(t, pool, `INSERT INTO leases(id,request_id,gateway_id,model_id,replica_id,scopes,budget_keys,reserved_tokens,
		priority,principal_type,principal_id,project_id,deadline,heartbeat_at)
		VALUES($1,'restart-request',$2,$3,$4,'{}',ARRAY[$5 || '|' ||
		(SELECT window_start::text FROM counters WHERE key=$5)],100,'standard','user',$6,$7,now(),now()-interval '2 minutes')`,
		lease, gateway, model, replica, key, principal, project)
	if n, err := admission.Reap(ctx, pool); err != nil || n != 1 {
		t.Fatalf("reaping before crash: %d, %v", n, err)
	}
	report := map[string]any{"lease": lease, "model": model, "route_id": replica, "observed": true,
		"lease_request": "restart-request", "lease_gateway": gateway, "lease_principal_type": "user",
		"lease_principal_id": principal, "lease_project_id": project, "prompt": 3, "completion": 4,
		"cached": 0, "cost": 0, "protocol": "openai.chat", "status": 200, "outcome": "ok",
		"queue_ms": 0, "ttft_ms": 0, "duration_ms": 0}
	var state string
	if err := pool.QueryRow(ctx, "SELECT starlight_settle_result($1)", report).Scan(&state); err != nil || state != "late" {
		t.Fatalf("acknowledging late observation: %s, %v", state, err)
	}
	var charged, reserved int64
	if err := pool.QueryRow(ctx, "SELECT used,reserved FROM counters WHERE key=$1", key).Scan(&charged, &reserved); err != nil || charged != 100 || reserved != 0 {
		t.Fatalf("fixture charge before crash: used=%d reserved=%d: %v", charged, reserved, err)
	}
	pool.Close()
	pair.stopPrimary()
	pair.start(pair.primary, pair.primaryAddress)
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		child := osexec.CommandContext(ctx, program, "-test.run=^TestAccountingWorkerProcessReplay$", "-test.v")
		child.Env = append(os.Environ(), "STARLIGHT_ACCOUNTING_CHILD_URL="+pair.URL(), "STARLIGHT_ACCOUNTING_CHILD_REPORT="+string(raw))
		if out, err := child.CombinedOutput(); err != nil {
			t.Fatalf("fresh worker process: %v\n%s", err, out)
		}
	}
}

func TestAccountingWorkerProcessReplay(t *testing.T) {
	url := os.Getenv("STARLIGHT_ACCOUNTING_CHILD_URL")
	if url == "" {
		t.Skip("invoked only by the isolated accounting restart drill")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, url, db.Options{MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var report map[string]any
	if err := json.Unmarshal([]byte(os.Getenv("STARLIGHT_ACCOUNTING_CHILD_REPORT")), &report); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := pool.QueryRow(ctx, "SELECT starlight_settle_result($1)", report).Scan(&state); err != nil || state != "duplicate" {
		t.Fatalf("late report after restart=%s: %v", state, err)
	}
	if _, err := accounting.Drain(ctx, pool, 100); err != nil {
		t.Fatal(err)
	}
	verified, err := audit.Verify(ctx, pool)
	if err != nil || verified.Checked != 2 {
		t.Fatalf("restarted worker audit=%+v: %v", verified, err)
	}
	var observed, charged, reserved, leases, pending int64
	err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM late_usage),sum(used),sum(reserved),
		(SELECT count(*) FROM leases),(SELECT count(*) FROM accounting_audit WHERE audit_seq IS NULL) FROM counters`).
		Scan(&observed, &charged, &reserved, &leases, &pending)
	if err != nil || observed != 1 || charged != 100 || reserved != 0 || leases != 0 || pending != 0 {
		t.Fatalf("restart changed accounting: late=%d used=%d reserved=%d leases=%d pending=%d: %v",
			observed, charged, reserved, leases, pending, err)
	}
}

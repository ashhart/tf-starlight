package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ashhart/tf-starlight/internal/db"
	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/testutil"
)

func schemaTen(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testutil.DB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;
		CREATE TABLE schema_migrations(version int PRIMARY KEY,name text NOT NULL,sha256 text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	all, err := db.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all[:10] {
		if _, err := pool.Exec(ctx, m.SQL); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "INSERT INTO schema_migrations(version,name,sha256) VALUES($1,$2,$3)", m.Version, m.Name, m.Sum); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

func TestTerminalMigrationPreservesAnExistingLeaseAndLegacyVoidCall(t *testing.T) {
	pool := schemaTen(t)
	ctx := context.Background()
	lease, model, replica, project, principal := ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
	if _, err := pool.Exec(ctx, `INSERT INTO leases(id,request_id,gateway_id,model_id,replica_id,scopes,priority,deadline)
		VALUES($1,'upgrade',$2,$3,$4,'{}','standard',now())`, lease, ids.New(), model, replica); err != nil {
		t.Fatal(err)
	}
	applied, err := db.Migrate(ctx, pool)
	if err != nil || len(applied) != 1 || applied[0] != "0011_terminal_usage.sql" {
		t.Fatalf("schema10 upgrade=%v: %v", applied, err)
	}
	r := map[string]any{"lease": lease, "tokens": 7, "micros": 0, "usage": ids.New(), "request": "upgrade",
		"principal_type": "user", "principal_id": principal, "user_id": principal, "project_id": project,
		"api_key_id": "", "task_id": "", "model": model, "model_name": "m", "route_kind": "local", "route_id": replica,
		"node_id": "", "protocol": "openai.chat", "status": 200, "outcome": "ok", "prompt": 3, "completion": 4,
		"cached": 0, "cost": 0, "queue_ms": 0, "ttft_ms": 0, "duration_ms": 0, "classification": "internal",
		"priority": "standard", "client": ""}
	for range 2 {
		if _, err := pool.Exec(ctx, "SELECT starlight_settle($1)", r); err != nil {
			t.Fatalf("already-running schema10 void call: %v", err)
		}
	}
	var events, receipts int
	var observed *bool
	err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_events),count(*),bool_or(observed) FROM terminal_leases`).
		Scan(&events, &receipts, &observed)
	if err != nil || events != 1 || receipts != 1 || observed != nil {
		t.Fatalf("legacy settlement events=%d receipts=%d observed=%v: %v", events, receipts, observed, err)
	}
	var state string
	r["observed"] = true
	if err := pool.QueryRow(ctx, "SELECT starlight_settle_result($1)", r).Scan(&state); err != nil || state != "unverified" {
		t.Fatalf("legacy gone report without authority=%s: %v", state, err)
	}
}

func TestTerminalMigrationRollsBackAndCanRetry(t *testing.T) {
	pool := schemaTen(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "CREATE TABLE accounting_audit (conflict int)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, pool); err == nil {
		t.Fatal("migration accepted a conflicting existing object")
	}
	var terminal, late *string
	var top int
	err := pool.QueryRow(ctx, `SELECT to_regclass('terminal_leases')::text,to_regclass('late_usage')::text,
		(SELECT max(version) FROM schema_migrations)`).Scan(&terminal, &late, &top)
	if err != nil || terminal != nil || late != nil || top != 10 {
		t.Fatalf("failed migration leaked objects: terminal=%v late=%v top=%d: %v", terminal, late, top, err)
	}
	if _, err := pool.Exec(ctx, "DROP TABLE accounting_audit"); err != nil {
		t.Fatal(err)
	}
	if applied, err := db.Migrate(ctx, pool); err != nil || len(applied) != 1 {
		t.Fatalf("migration retry=%v: %v", applied, err)
	}
}

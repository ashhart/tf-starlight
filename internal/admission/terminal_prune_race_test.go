package admission_test

import (
	"context"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/accounting"
	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/levels"
)

func TestLateTransactionSurvivesExpiryAndConcurrentPrune(t *testing.T) {
	f := newFleet(t, 1)
	a := f.admitter()
	req := f.request(levels.Standard, 5*time.Second)
	lease, err := a.Admit(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	reapLease(t, f, lease)
	clone := ids.New()
	f.exec(t, `WITH source AS (SELECT *,now()-interval '7 days'+interval '2 seconds' AS ended
		FROM terminal_leases WHERE lease_id=$1), body AS (SELECT *,
		(receipt::jsonb||jsonb_build_object('lease',$2::uuid,'ended_at',ended,'late_until',ended+interval '7 days'))::text AS frozen FROM source)
		INSERT INTO terminal_leases(lease_id,project_id,kind,charged_tokens,charged_micros,observed,ended_at,late_until,receipt,receipt_hash)
		SELECT $2,project_id,kind,charged_tokens,charged_micros,observed,ended,ended+interval '7 days',frozen,
		sha256(convert_to(frozen,'UTF8')) FROM body`, lease.ID, clone)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	late, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer late.Rollback(context.Background())
	report := map[string]any{"lease": clone, "model": f.model, "route_id": lease.Target.ReplicaID, "observed": true,
		"lease_request": req.RequestID, "lease_gateway": a.GatewayID, "lease_principal_type": "",
		"lease_principal_id": "", "lease_project_id": "", "prompt": 3, "completion": 4, "cached": 0, "cost": 0,
		"protocol": "openai.chat", "status": 200, "outcome": "ok", "queue_ms": 0, "ttft_ms": 0, "duration_ms": 0}
	var state string
	if err := late.QueryRow(ctx, "SELECT starlight_settle_result($1)", report).Scan(&state); err != nil || state != "late" {
		t.Fatalf("late transaction before horizon=%s: %v", state, err)
	}
	var until time.Time
	if err := late.QueryRow(ctx, "SELECT late_until FROM terminal_leases WHERE lease_id=$1", clone).Scan(&until); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(max(0, time.Until(until)+20*time.Millisecond))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-timer.C:
	}
	if n, err := accounting.Prune(ctx, f.pool); err != nil || n != 0 {
		t.Fatalf("prune waited for or deleted an in-progress late receipt: %d, %v", n, err)
	}
	normal, err := a.Admit(ctx, f.request(levels.Standard, 2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Settle(ctx, normal, admission.Usage{Prompt: 1, Observed: true}, record()); err != nil {
		t.Fatal("unrelated normal settlement blocked by a late receipt", err)
	}
	if err := late.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := accounting.Drain(ctx, f.pool, 100); err != nil {
		t.Fatal(err)
	}
	if n, err := accounting.Prune(ctx, f.pool); err != nil || n != 0 {
		t.Fatalf("prune lost the newly committed 90-day observation: %d, %v", n, err)
	}
	var reports int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM late_usage WHERE lease_id=$1", clone).Scan(&reports); err != nil || reports != 1 {
		t.Fatalf("late observation lost at expiry: %d, %v", reports, err)
	}
}

func TestPruneReceiptLockDoesNotBlockAnotherLeaseSettlement(t *testing.T) {
	f := newFleet(t, 1)
	a := f.admitter()
	lease := reservedLease(t, f, a)
	reapLease(t, f, lease)
	expired := agedTerminal(t, f, lease, 8, -1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := accounting.Drain(ctx, f.pool, 100); err != nil {
		t.Fatal(err)
	}
	pruner, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pruner.Rollback(context.Background())
	if _, err := pruner.Exec(ctx, "SELECT lease_id FROM terminal_leases WHERE lease_id=$1 FOR UPDATE", expired.ID); err != nil {
		t.Fatal(err)
	}
	late := make(chan error, 1)
	go func() {
		state, err := a.SettleResult(ctx, expired, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record())
		if err == nil && state != "unknown" {
			t.Errorf("pruned lease returned %s", state)
		}
		late <- err
	}()
	eventually(t, "late report waits for the receipt lock", func() bool { return active(t, f.pool, "starlight_settle_result(", true) > 0 })
	normal := reservedLease(t, f, a)
	if err := a.Settle(ctx, normal, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record()); err != nil {
		t.Fatal("unrelated normal settlement blocked by prune", err)
	}
	for _, table := range []string{"accounting_audit", "terminal_leases"} {
		if _, err := pruner.Exec(ctx, "DELETE FROM "+table+" WHERE lease_id=$1", expired.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruner.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-late; err != nil {
		t.Fatal(err)
	}
	assertSettlementTotals(t, f, 1, 7, 107)
}

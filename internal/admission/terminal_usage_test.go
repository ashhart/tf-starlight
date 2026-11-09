package admission_test

import (
	"context"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/admission"
)

func TestReapedLeaseKeepsOneLateObservationWithoutAnotherCharge(t *testing.T) {
	f := newFleet(t, 1)
	a := f.admitter()
	lease := reservedLease(t, f, a)
	f.exec(t, "UPDATE leases SET heartbeat_at=now()-interval '2 minutes' WHERE id=$1", lease.ID)
	if n, err := admission.Reap(context.Background(), f.pool); err != nil || n != 1 {
		t.Fatalf("reaping fixture: %d, %v", n, err)
	}
	for range 2 {
		if err := a.Settle(context.Background(), lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record()); err != nil {
			t.Fatal(err)
		}
	}
	assertSettlementTotals(t, f, 0, 0, 100)
	var rows int
	var outcome string
	var charged, observed int64
	err := f.pool.QueryRow(context.Background(), `SELECT count(*), min(t.kind), min(t.charged_tokens),
		min((l.report::jsonb->>'prompt')::bigint+(l.report::jsonb->>'completion')::bigint)
		FROM terminal_leases t JOIN late_usage l USING (lease_id) WHERE lease_id=$1`, lease.ID).
		Scan(&rows, &outcome, &charged, &observed)
	if err != nil || rows != 1 || outcome != "reaped" || charged != 100 || observed != 7 {
		t.Fatalf("late history rows=%d outcome=%s charged=%d observed=%d: %v", rows, outcome, charged, observed, err)
	}
	var ended, received time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT t.ended_at,l.created_at
		FROM terminal_leases t JOIN late_usage l USING (lease_id) WHERE lease_id=$1`, lease.ID).Scan(&ended, &received); err != nil || received.Before(ended) {
		t.Fatalf("terminal/late timestamps: %v", err)
	}
}

func TestFailedReaperChargeRetainsLeaseWithoutTerminalEvidence(t *testing.T) {
	f := newFleet(t, 1)
	a := f.admitter()
	lease := reservedLease(t, f, a)
	f.exec(t, "UPDATE leases SET heartbeat_at=now()-interval '2 minutes' WHERE id=$1", lease.ID)
	f.exec(t, "ALTER TABLE counters ADD CONSTRAINT reject_reaped_charge CHECK (used < 50)")
	if _, err := admission.Reap(context.Background(), f.pool); err == nil {
		t.Fatal("injected reaper counter failure succeeded")
	}
	var leases, receipts, pending, charged, reserved int64
	err := f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM leases),
		(SELECT count(*) FROM terminal_leases),(SELECT count(*) FROM accounting_audit),used,reserved
		FROM counters WHERE key='tokens:day:project:once'`).Scan(&leases, &receipts, &pending, &charged, &reserved)
	if err != nil || leases != 1 || receipts != 0 || pending != 0 || charged != 0 || reserved != 100 {
		t.Fatalf("reaper rollback leases=%d receipts=%d pending=%d used=%d reserved=%d: %v",
			leases, receipts, pending, charged, reserved, err)
	}
}

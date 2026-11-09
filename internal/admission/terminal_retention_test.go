package admission_test

import (
	"context"
	"testing"

	"github.com/ashhart/tf-starlight/internal/accounting"
	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/ids"
)

// agedTerminal copies captured evidence to an aged lease without a mutable production clock.
func agedTerminal(t *testing.T, f fleet, original *admission.Lease, days, lateDays int) *admission.Lease {
	t.Helper()
	lease := *original
	lease.ID = ids.New()
	f.exec(t, `WITH source AS (SELECT *,now()-make_interval(days=>$3) AS ended FROM terminal_leases WHERE lease_id=$1),
		body AS (SELECT *, (receipt::jsonb || jsonb_build_object('lease',$2::uuid,'ended_at',ended,
		'late_until',ended+interval '7 days'))::text AS frozen FROM source)
		INSERT INTO terminal_leases (lease_id,project_id,kind,charged_tokens,charged_micros,observed,ended_at,late_until,receipt,receipt_hash)
		SELECT $2,project_id,kind,charged_tokens,charged_micros,observed,ended,ended+interval '7 days',frozen,
		sha256(convert_to(frozen,'UTF8')) FROM body`, original.ID, lease.ID, days)
	f.exec(t, "INSERT INTO accounting_audit (lease_id,kind) VALUES ($1,'terminal')", lease.ID)
	if lateDays >= 0 {
		f.exec(t, `WITH body AS (SELECT (l.report::jsonb || jsonb_build_object('lease',$2::uuid,
			'terminal_hash',encode(t.receipt_hash,'hex')))::text AS frozen
			FROM late_usage l JOIN terminal_leases t ON t.lease_id=$2 WHERE l.lease_id=$1)
			INSERT INTO late_usage (lease_id,created_at,report,report_hash)
			SELECT $2,now()-make_interval(days=>$3),frozen,sha256(convert_to(frozen,'UTF8')) FROM body`, original.ID, lease.ID, lateDays)
		f.exec(t, "INSERT INTO accounting_audit (lease_id,kind) VALUES ($1,'late')", lease.ID)
	}
	return &lease
}

func TestTerminalRetentionPreservesPendingAndLateEvidence(t *testing.T) {
	f := newFleet(t, 1)
	a := f.admitter()
	lease := reservedLease(t, f, a)
	reapLease(t, f, lease)
	u, rec := admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record()
	if err := a.Settle(context.Background(), lease, u, rec); err != nil {
		t.Fatal(err)
	}
	expired := agedTerminal(t, f, lease, 8, -1)
	recent := agedTerminal(t, f, lease, 30, 20)
	old := agedTerminal(t, f, lease, 100, 95)
	ctx := context.Background()
	if state, err := a.SettleResult(ctx, expired, u, rec); err != nil || state != "expired" {
		t.Fatalf("first report past horizon=%s: %v", state, err)
	}
	if state, err := a.SettleResult(ctx, recent, u, rec); err != nil || state != "duplicate" {
		t.Fatalf("retained report replay=%s: %v", state, err)
	}
	if n, err := accounting.Prune(ctx, f.pool); err != nil || n != 0 {
		t.Fatalf("pending audit was pruned=%d: %v", n, err)
	}
	if _, err := f.pool.Exec(ctx, "DELETE FROM accounting_audit WHERE lease_id=$1", old.ID); err == nil {
		t.Fatal("pending audit could be discarded")
	}
	if _, err := accounting.Drain(ctx, f.pool, 100); err != nil {
		t.Fatal(err)
	}
	if n, err := accounting.Prune(ctx, f.pool); err != nil || n != 2 {
		t.Fatalf("expired receipts pruned=%d, want2: %v", n, err)
	}
	for _, gone := range []*admission.Lease{expired, old} {
		if state, err := a.SettleResult(ctx, gone, u, rec); err != nil || state != "unknown" {
			t.Fatalf("pruned report replay=%s: %v", state, err)
		}
	}
	var retained, chained int
	if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM terminal_leases WHERE lease_id=$1),
		(SELECT count(*) FROM audit_events WHERE target_id=$2)`, recent.ID, old.ID).Scan(&retained, &chained); err != nil || retained != 1 || chained != 2 {
		t.Fatalf("late retention=%d durable chain=%d: %v", retained, chained, err)
	}
	assertSettlementTotals(t, f, 0, 0, 100)
}

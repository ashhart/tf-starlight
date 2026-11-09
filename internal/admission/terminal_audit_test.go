package admission_test

import (
	"context"
	"encoding/hex"
	"strconv"
	"sync"
	"testing"

	"github.com/ashhart/tf-starlight/internal/accounting"
	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/audit"
)

func TestLateAuditRollbackRetryAndImmutableLink(t *testing.T) {
	f := newFleet(t, 1)
	a := f.admitter()
	lease := reservedLease(t, f, a)
	reapLease(t, f, lease)
	if err := a.Settle(context.Background(), lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record()); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `CREATE FUNCTION reject_late_audit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.action='accounting.late' THEN RAISE EXCEPTION 'injected audit failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER reject_late BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_late_audit()`)
	ctx := context.Background()
	if n, err := accounting.Drain(ctx, f.pool, 100); err == nil || n != 0 {
		t.Fatalf("failed audit transaction returned %d, %v", n, err)
	}
	var events, pending int
	if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM audit_events),
		(SELECT count(*) FROM accounting_audit WHERE audit_seq IS NULL)`).Scan(&events, &pending); err != nil || events != 0 || pending != 2 {
		t.Fatalf("audit failure lost atomicity events=%d pending=%d: %v", events, pending, err)
	}
	assertSettlementTotals(t, f, 0, 0, 100)
	f.exec(t, "DROP TRIGGER reject_late ON audit_events")
	if n, err := accounting.Drain(ctx, f.pool, 100); err != nil || n != 0 {
		t.Fatalf("failed lease did not respect retry backoff: %d, %v", n, err)
	}
	f.exec(t, "UPDATE accounting_audit SET next_attempt_at=now() WHERE audit_seq IS NULL")
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			if _, err := accounting.Drain(ctx, f.pool, 100); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	verified, err := audit.Verify(ctx, f.pool)
	if err != nil || verified.Checked != 2 {
		t.Fatalf("audit chain=%+v: %v", verified, err)
	}
	var seq int64
	var hash []byte
	var linkedSeq, linkedHash string
	err = f.pool.QueryRow(ctx, `SELECT a.audit_seq,a.audit_hash,e.details->>'terminal_audit_seq',e.details->>'terminal_audit_hash'
		FROM accounting_audit a JOIN audit_events e ON e.action='accounting.late' AND e.target_id=a.lease_id::text
		WHERE a.lease_id=$1 AND a.kind='terminal'`, lease.ID).Scan(&seq, &hash, &linkedSeq, &linkedHash)
	if err != nil || linkedSeq != strconv.FormatInt(seq, 10) || linkedHash != hex.EncodeToString(hash) {
		t.Fatalf("immutable terminal link %d/%s: %v", seq, linkedSeq, err)
	}
	for _, statement := range []string{
		"UPDATE terminal_leases SET kind='released' WHERE lease_id=$1",
		"UPDATE late_usage SET report=report WHERE lease_id=$1",
		"UPDATE accounting_audit SET audit_seq=audit_seq+1 WHERE lease_id=$1",
	} {
		if _, err := f.pool.Exec(ctx, statement, lease.ID); err == nil {
			t.Fatalf("immutable evidence accepted %s", statement)
		}
	}
	if n, err := accounting.Drain(ctx, f.pool, 100); err != nil || n != 0 {
		t.Fatalf("linked events replayed: %d, %v", n, err)
	}
}

func TestUnreportedReapedLeaseHasAnAuditableTerminalEvent(t *testing.T) {
	f := newFleet(t, 1)
	a := f.admitter()
	lease := reservedLease(t, f, a)
	reapLease(t, f, lease)
	if n, err := accounting.Drain(context.Background(), f.pool, 1); err != nil || n != 1 {
		t.Fatalf("unreported terminal audit=%d: %v", n, err)
	}
	var kind string
	if err := f.pool.QueryRow(context.Background(), `SELECT details->>'receipt' FROM audit_events WHERE action='accounting.terminal'`).
		Scan(&kind); err != nil || kind == "" {
		t.Fatalf("dead gateway's terminal charge is missing: %v", err)
	}
	for _, limit := range []int{0, 101} {
		if _, err := accounting.Drain(context.Background(), f.pool, limit); err == nil {
			t.Fatalf("unbounded drain accepted %d", limit)
		}
	}
}

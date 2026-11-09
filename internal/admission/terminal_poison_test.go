package admission_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/accounting"
	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/audit"
)

func TestPoisonAuditLeaseBacksOffWhileHealthyLeaseCommits(t *testing.T) {
	f := newFleet(t, 2)
	a := f.admitter()
	first, healthy := reservedLease(t, f, a), reservedLease(t, f, a)
	reapLease(t, f, first)
	reapLease(t, f, healthy)
	for _, lease := range []*admission.Lease{first, healthy} {
		if err := a.Settle(context.Background(), lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record()); err != nil {
			t.Fatal(err)
		}
	}
	f.exec(t, `CREATE TABLE rejected_lease(id text PRIMARY KEY)`)
	f.exec(t, `INSERT INTO rejected_lease VALUES($1)`, first.ID)
	f.exec(t, `CREATE FUNCTION reject_poison() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF EXISTS(SELECT 1 FROM rejected_lease WHERE id=NEW.target_id) THEN RAISE EXCEPTION 'injected'; END IF;
		RETURN NEW; END $$;
		CREATE TRIGGER reject_poison BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_poison()`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, err := accounting.DrainUntil(ctx, f.pool)
	if err == nil || r.Appended != 2 || r.Failed != 1 {
		t.Fatalf("one failure stalled healthy work: %+v, %v", r, err)
	}
	var attempts int
	var future bool
	err = f.pool.QueryRow(context.Background(), `SELECT min(attempts),bool_and(next_attempt_at>now()+interval '20 seconds')
		FROM accounting_audit WHERE lease_id=$1`, first.ID).Scan(&attempts, &future)
	if err != nil || attempts != 1 || !future {
		t.Fatalf("durable retry state attempts=%d future=%v: %v", attempts, future, err)
	}
	f.exec(t, "DROP TRIGGER reject_poison ON audit_events")
	f.exec(t, "UPDATE accounting_audit SET next_attempt_at=now() WHERE lease_id=$1 AND audit_seq IS NULL", first.ID)
	if n, err := accounting.Drain(context.Background(), f.pool, 100); err != nil || n != 2 {
		t.Fatalf("operator retry did not recover the same lease: %d, %v", n, err)
	}
	if v, err := audit.Verify(context.Background(), f.pool); err != nil || v.Checked != 4 {
		t.Fatalf("recovered audit chain=%+v, %v", v, err)
	}
	assertSettlementTotals(t, f, 0, 0, 200)
}

func TestAccountingDrainAndOrdinaryAuditWriterShareOneChain(t *testing.T) {
	f := newFleet(t, 1)
	a := f.admitter()
	lease := reservedLease(t, f, a)
	reapLease(t, f, lease)
	if err := a.Settle(context.Background(), lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 21)
	for range 20 {
		wg.Go(func() {
			e := audit.Actor(nil)
			e.Action, e.TargetType, e.TargetID = "test.non_accounting", "fixture", "ordinary"
			errs <- audit.Log(context.Background(), f.pool, e)
		})
	}
	wg.Go(func() { _, err := accounting.Drain(context.Background(), f.pool, 100); errs <- err }())
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	verified, err := audit.Verify(context.Background(), f.pool)
	if err != nil || verified.Checked != 22 {
		t.Fatalf("mixed-writer audit chain=%+v: %v", verified, err)
	}
	var linked int
	err = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM accounting_audit a JOIN audit_events e
		ON e.seq=a.audit_seq AND e.hash=a.audit_hash AND e.target_id=a.lease_id::text AND e.action='accounting.'||a.kind`).Scan(&linked)
	if err != nil || linked != 2 {
		t.Fatalf("accounting linked another writer's event: %d, %v", linked, err)
	}
}

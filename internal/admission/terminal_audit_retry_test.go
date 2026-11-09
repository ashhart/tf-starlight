package admission_test

import (
	"context"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/accounting"
	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/audit"
)

func TestAccountingDrainRetryCountsOnlyCommittedEvents(t *testing.T) {
	f := newFleet(t, 1)
	a := f.admitter()
	lease := reservedLease(t, f, a)
	reapLease(t, f, lease)
	if err := a.Settle(context.Background(), lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record()); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `CREATE SEQUENCE audit_retry_once;
		CREATE FUNCTION retry_late_audit_link() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.kind='late' AND NEW.audit_seq IS NOT NULL THEN
				IF nextval('audit_retry_once')=1 THEN
					RAISE EXCEPTION 'one-shot audit retry fixture' USING ERRCODE='40001';
				END IF;
			END IF;
			RETURN NEW;
		END $$;
		CREATE TRIGGER retry_late_audit_link BEFORE UPDATE ON accounting_audit
		FOR EACH ROW EXECUTE FUNCTION retry_late_audit_link()`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	appended, err := accounting.Drain(ctx, f.pool, 2)
	var persisted, pending, lateAttempts int
	queryErr := f.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM audit_events WHERE target_id=$1),
		(SELECT count(*) FROM accounting_audit WHERE lease_id::text=$1 AND audit_seq IS NULL),
		(SELECT last_value FROM audit_retry_once)`, lease.ID).Scan(&persisted, &pending, &lateAttempts)
	if err != nil || queryErr != nil || appended != 2 || persisted != 2 || pending != 0 || lateAttempts != 2 {
		t.Fatalf("retry counts must describe committed events: reported=%d persisted=%d pending=%d late_attempts=%d drain=%v query=%v",
			appended, persisted, pending, lateAttempts, err, queryErr)
	}
	verified, err := audit.Verify(ctx, f.pool)
	if err != nil || verified.Checked != 2 {
		t.Fatalf("retried audit chain changed: %+v, %v", verified, err)
	}
	assertSettlementTotals(t, f, 0, 0, 100)
}

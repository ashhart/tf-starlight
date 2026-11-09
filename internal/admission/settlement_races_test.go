package admission_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/levels"
)

func reservedLease(t *testing.T, f fleet, a *admission.Admitter) *admission.Lease {
	t.Helper()
	req := f.request(levels.Standard, 5*time.Second)
	req.Budgets = []admission.Counter{{Key: "tokens:day:project:once", Window: time.Now().UTC().Truncate(24 * time.Hour), Limit: 10000, Amount: 100}}
	lease, err := a.Admit(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func assertSettlementTotals(t *testing.T, f fleet, events, tokens, used int64) {
	t.Helper()
	var rows, sum, charged, reserved, leases int64
	err := f.pool.QueryRow(context.Background(), `SELECT count(*), coalesce(sum(prompt_tokens+completion_tokens),0),
		(SELECT used FROM counters WHERE key='tokens:day:project:once'),
		(SELECT reserved FROM counters WHERE key='tokens:day:project:once'), (SELECT count(*) FROM leases) FROM usage_events`).
		Scan(&rows, &sum, &charged, &reserved, &leases)
	if err != nil || rows != events || sum != tokens || charged != used || reserved != 0 || leases != 0 {
		t.Fatalf("settlement totals events=%d tokens=%d used=%d reserved=%d leases=%d, err=%v", rows, sum, charged, reserved, leases, err)
	}
}

func TestConcurrentSettlementRecordsOneUsageAndBudgetEffect(t *testing.T) {
	f := newFleet(t, 1)
	a := f.admitter()
	lease := reservedLease(t, f, a)
	rec := record()
	start := make(chan struct{})
	errs := make(chan error, 12)
	var workers sync.WaitGroup
	for range 12 {
		workers.Go(func() {
			<-start
			errs <- a.Settle(context.Background(), lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, rec)
		})
	}
	close(start)
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertSettlementTotals(t, f, 1, 7, 7)
}

func TestGoneLeasesCannotRecordUsage(t *testing.T) {
	for _, gone := range []string{"released", "reaped"} {
		t.Run(gone, func(t *testing.T) {
			f := newFleet(t, 1)
			a := f.admitter()
			lease := reservedLease(t, f, a)
			used := int64(0)
			if gone == "released" {
				f.exec(t, "SELECT starlight_release($1,0,0)", lease.ID)
			} else {
				f.exec(t, "UPDATE leases SET heartbeat_at=now()-interval '2 minutes' WHERE id=$1", lease.ID)
				if n, err := admission.Reap(context.Background(), f.pool); err != nil || n != 1 {
					t.Fatalf("reaping the fixture lease: %d, %v", n, err)
				}
				used = 100
			}
			if err := a.Settle(context.Background(), lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record()); err != nil {
				t.Fatal(err)
			}
			assertSettlementTotals(t, f, 0, 0, used)
		})
	}
}

func TestDifferentLeasesWithTheSameClientRequestIDRemainDistinct(t *testing.T) {
	f := newFleet(t, 2)
	a := f.admitter()
	first, second := reservedLease(t, f, a), reservedLease(t, f, a)
	rec := record()
	for _, lease := range []*admission.Lease{first, second} {
		if err := a.Settle(context.Background(), lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, rec); err != nil {
			t.Fatal(err)
		}
	}
	assertSettlementTotals(t, f, 2, 14, 14)
}

func TestFailedUsageInsertRetainsTheLeaseAndReservation(t *testing.T) {
	f := newFleet(t, 1)
	a := f.admitter()
	lease := reservedLease(t, f, a)
	rec := record()
	rec.ProjectID = "invalid-uuid"
	if err := a.Settle(context.Background(), lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, rec); err == nil {
		t.Fatal("invalid usage unexpectedly committed")
	}
	var leases, used, reserved int64
	err := f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM leases), used, reserved
		FROM counters WHERE key='tokens:day:project:once'`).Scan(&leases, &used, &reserved)
	if err != nil || leases != 1 || used != 0 || reserved != 100 {
		t.Fatalf("failed settlement leaked changes: leases=%d used=%d reserved=%d, %v", leases, used, reserved, err)
	}
	var receipts, pending int
	err = f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM terminal_leases),
		(SELECT count(*) FROM accounting_audit)`).Scan(&receipts, &pending)
	if err != nil || receipts != 0 || pending != 0 {
		t.Fatalf("failed settlement leaked terminal evidence: receipts=%d pending=%d, %v", receipts, pending, err)
	}
	if err := a.Settle(context.Background(), lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record()); err != nil {
		t.Fatal(err)
	}
	assertSettlementTotals(t, f, 1, 7, 7)
}

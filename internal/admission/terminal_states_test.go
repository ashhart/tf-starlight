package admission_test

import (
	"context"
	"sync"
	"testing"

	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/ids"
)

func reapLease(t *testing.T, f fleet, lease *admission.Lease) {
	t.Helper()
	f.exec(t, "UPDATE leases SET heartbeat_at=now()-interval '2 minutes' WHERE id=$1", lease.ID)
	if n, err := admission.Reap(context.Background(), f.pool); err != nil || n != 1 {
		t.Fatalf("reaping fixture: %d, %v", n, err)
	}
}

func TestTerminalUsageStatesDoNotInventWork(t *testing.T) {
	for _, kind := range []string{"released", "unknown", "unobserved", "complete", "legacy_deleted"} {
		t.Run(kind, func(t *testing.T) {
			f := newFleet(t, 1)
			a := f.admitter()
			lease := reservedLease(t, f, a)
			u := admission.Usage{Prompt: 3, Completion: 4, Observed: true}
			rec := record()
			expect := kind
			switch kind {
			case "released":
				f.exec(t, "SELECT starlight_release($1,0,0)", lease.ID)
			case "unknown":
				lease.ID = ids.New()
			case "unobserved":
				reapLease(t, f, lease)
				u.Observed = false
			case "complete":
				if err := a.Settle(context.Background(), lease, u, rec); err != nil {
					t.Fatal(err)
				}
			case "legacy_deleted":
				f.exec(t, "DELETE FROM leases WHERE id=$1", lease.ID)
				expect = "late"
			}
			state, err := a.SettleResult(context.Background(), lease, u, rec)
			if err != nil || state != expect {
				t.Fatalf("state=%s, want %s: %v", state, expect, err)
			}
			var rows int
			if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM late_usage").Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if kind == "legacy_deleted" {
				var charged *int64
				if err := f.pool.QueryRow(context.Background(), "SELECT charged_tokens FROM terminal_leases WHERE lease_id=$1", lease.ID).
					Scan(&charged); err != nil || charged != nil || rows != 1 {
					t.Fatalf("legacy charge must remain unknown: %v, late=%d, %v", charged, rows, err)
				}
			} else if rows != 0 {
				t.Fatalf("%s minted %d late rows", kind, rows)
			}
		})
	}
}

func TestConcurrentLateReportsAreIdempotentAndConflictsFail(t *testing.T) {
	f := newFleet(t, 1)
	a := f.admitter()
	lease := reservedLease(t, f, a)
	reapLease(t, f, lease)
	u, rec := admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record()
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 12 {
		wg.Go(func() {
			state, err := a.SettleResult(context.Background(), lease, u, rec)
			if err == nil && state != "late" && state != "duplicate" {
				t.Errorf("concurrent state=%s", state)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	u.Completion++
	if _, err := a.SettleResult(context.Background(), lease, u, rec); err == nil {
		t.Fatal("conflicting late report succeeded")
	}
	u.Completion = -1
	if _, err := a.SettleResult(context.Background(), lease, u, rec); err == nil {
		t.Fatal("negative late report succeeded")
	}
	u.Completion = 4
	a.GatewayID = ids.New()
	if _, err := a.SettleResult(context.Background(), lease, u, rec); err == nil {
		t.Fatal("another gateway's report succeeded")
	}
	assertSettlementTotals(t, f, 0, 0, 100)
	var rows int
	if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM late_usage").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("concurrent late rows=%d: %v", rows, err)
	}
}

func TestConservativeNormalSettlementCanReceiveALateObservation(t *testing.T) {
	f := newFleet(t, 1)
	a := f.admitter()
	lease := reservedLease(t, f, a)
	if err := a.Settle(context.Background(), lease, admission.Usage{}, record()); err != nil {
		t.Fatal(err)
	}
	state, err := a.SettleResult(context.Background(), lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record())
	if err != nil || state != "late" {
		t.Fatalf("conservative settlement late state=%s: %v", state, err)
	}
	assertSettlementTotals(t, f, 1, 100, 100)
}

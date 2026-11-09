package admission_test

import (
	"context"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/admission"
)

func TestSettlementAndReapingSerializeOnTheLeaseBeforeCounters(t *testing.T) {
	for attempt := range 8 {
		t.Run(string(rune('a'+attempt)), func(t *testing.T) {
			f := newFleet(t, 1)
			a := f.admitter()
			a.Timeout = 10 * time.Second
			lease := reservedLease(t, f, a)
			f.exec(t, "UPDATE leases SET heartbeat_at=now()-interval '2 minutes' WHERE id=$1", lease.ID)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			blocker, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, "SELECT id FROM leases WHERE id=$1 FOR UPDATE", lease.ID); err != nil {
				t.Fatal(err)
			}
			type reaped struct {
				n   int
				err error
			}
			settled := make(chan error, 1)
			reaper := make(chan reaped, 1)
			go func() {
				settled <- a.Settle(ctx, lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record())
			}()
			go func() {
				n, err := admission.Reap(ctx, f.pool)
				reaper <- reaped{n, err}
			}()
			eventually(t, "both terminal paths wait for the same lease", func() bool {
				return active(t, f.pool, "starlight_settle(", true) > 0 && active(t, f.pool, "starlight_reap_stale(", true) > 0
			})
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-settled; err != nil {
				t.Fatalf("settlement deadlocked or failed: %v", err)
			}
			r := <-reaper
			if r.err != nil || r.n < 0 || r.n > 1 {
				t.Fatalf("reaper deadlocked or failed: %+v", r)
			}
			if r.n == 1 {
				assertSettlementTotals(t, f, 0, 0, 100)
			} else {
				assertSettlementTotals(t, f, 1, 7, 7)
			}
			var receipts, late int
			err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM terminal_leases),
				(SELECT count(*) FROM late_usage)`).Scan(&receipts, &late)
			if err != nil || receipts != 1 || late != r.n {
				t.Fatalf("terminal race receipts=%d late=%d reaped=%d: %v", receipts, late, r.n, err)
			}
		})
	}
}

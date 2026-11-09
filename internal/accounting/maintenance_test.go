package accounting_test

import (
	"context"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/accounting"
	"github.com/ashhart/tf-starlight/internal/testutil"
)

func TestPruneUntilDrainsMultipleBatchesWithoutAPerPassCeiling(t *testing.T) {
	pool := testutil.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `INSERT INTO terminal_leases(lease_id,kind,charged_tokens,charged_micros,observed,
		ended_at,late_until,receipt,receipt_hash)
		SELECT gen_random_uuid(),'settled',0,0,true,now()-interval '8 days',now()-interval '1 day','{}',
		sha256(convert_to('{}','UTF8')) FROM generate_series(1,6001)`)
	if err != nil {
		t.Fatal(err)
	}
	result, err := accounting.PruneUntil(ctx, pool)
	if err != nil || result.Deleted != 6001 || result.Batches != 7 || result.BudgetExhausted {
		t.Fatalf("cleanup cannot catch up across batches: %+v, %v", result, err)
	}
	if _, err := accounting.PruneUntil(context.Background(), pool); err == nil {
		t.Fatal("unbounded maintenance context accepted")
	}
	if _, err := accounting.DrainUntil(context.Background(), pool); err == nil {
		t.Fatal("unbounded drain context accepted")
	}
}

func TestMaintenanceBacklogKeepsUnknownCountsAndDeadlineStateExplicit(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO terminal_leases(lease_id,kind,charged_tokens,charged_micros,observed,
		ended_at,late_until,receipt,receipt_hash)
		SELECT gen_random_uuid(),'settled',0,0,true,now()-interval '8 days',now()-interval '1 day','{}',
		sha256(convert_to('{}','UTF8')) FROM generate_series(1,7)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO accounting_audit(lease_id,kind,created_at)
		SELECT lease_id,'terminal',now()-interval '1 minute' FROM terminal_leases LIMIT 2`); err != nil {
		t.Fatal(err)
	}
	b, err := accounting.ReadBacklog(ctx, pool)
	if err != nil || b.Pending != 2 || b.Expired != 5 || b.PendingCapped || b.ExpiredCapped || b.OldestPending < 60 {
		t.Fatalf("backlog snapshot=%+v: %v", b, err)
	}
	expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()
	r, err := accounting.PruneUntil(expired, pool)
	if err != nil || !r.BudgetExhausted || r.Deleted != 0 {
		t.Fatalf("deadline state=%+v: %v", r, err)
	}
}

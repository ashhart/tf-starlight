package accounting_test

import (
	"context"
	"testing"

	"github.com/ashhart/tf-starlight/internal/accounting"
	"github.com/ashhart/tf-starlight/internal/testutil"
)

func TestReceiptPruningIsBoundedPerPass(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO terminal_leases(lease_id,kind,charged_tokens,charged_micros,observed,
		ended_at,late_until,receipt,receipt_hash)
		SELECT gen_random_uuid(),'settled',0,0,true,now()-interval '8 days',now()-interval '1 day','{}',
		sha256(convert_to('{}','UTF8')) FROM generate_series(1,1001)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{1000, 1, 0} {
		if n, err := accounting.Prune(ctx, pool); err != nil || n != want {
			t.Fatalf("pruned=%d want=%d: %v", n, want, err)
		}
	}
}

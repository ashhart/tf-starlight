package admission_test

import (
	"context"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/levels"
)

func TestRepeatedSettlementRecordsOneUsageEvent(t *testing.T) {
	f := newFleet(t, 1)
	a := f.admitter()
	ctx := context.Background()
	lease, err := a.Admit(ctx, f.request(levels.Standard, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	rec := record()
	for range 2 {
		if err := a.Settle(ctx, lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, rec); err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM usage_events").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("one settled lease produced %d usage events, want 1", rows)
	}
}

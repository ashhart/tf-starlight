package admission_test

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/levels"
)

func TestSettlementLatencySample(t *testing.T) {
	if os.Getenv("STARLIGHT_SETTLEMENT_LATENCY") != "1" {
		t.Skip("set STARLIGHT_SETTLEMENT_LATENCY=1 for five 300-settlement latency samples")
	}
	f := newFleet(t, 1)
	a := f.admitter()
	ctx := context.Background()
	rec := record()
	for trial := range 5 {
		var samples []time.Duration
		for range 300 {
			lease, err := a.Admit(ctx, f.request(levels.Standard, 5*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			if err := a.Settle(ctx, lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, rec); err != nil {
				t.Fatal(err)
			}
			samples = append(samples, time.Since(start))
		}
		slices.Sort(samples)
		t.Logf("settlement trial=%d n=%d p50_ns=%d p95_ns=%d", trial, len(samples), samples[150], samples[285])
	}
}

package mcpgateway

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAnonymousSummaryHasFixedStorageAndRestoresFailedCounts(t *testing.T) {
	s := &refusalSummary{}
	var wait sync.WaitGroup
	for range 16 {
		wait.Go(func() {
			for range 100 {
				s.add(401)
				s.add(403)
				s.add(123456)
			}
		})
	}
	wait.Wait()
	s.flush(func(ctx context.Context, e Event) error {
		if ctx.Err() != nil || e.RefusalCounts["401"] != 1600 || e.RefusalCounts["403"] != 1600 || e.RefusalCounts["other"] != 1600 {
			t.Fatal("concurrent anonymous counters lost counts")
		}
		s.add(405)
		return errors.New("fixture store outage")
	}, nil)
	s.flush(func(ctx context.Context, e Event) error {
		deadline, ok := ctx.Deadline()
		if ctx.Err() != nil || !ok || time.Until(deadline) > time.Second || len(e.RefusalCounts) != 4 ||
			e.RefusalCounts["401"] != 1600 || e.RefusalCounts["403"] != 1600 || e.RefusalCounts["405"] != 1 || e.RefusalCounts["other"] != 1600 ||
			e.Actor != nil || e.AttemptID != "" || e.Tool != "" || e.Method != "" {
			t.Fatal("retry lost counts, cleanup bound or content-free summary")
		}
		return nil
	}, nil)
	s.flush(func(context.Context, Event) error { t.Fatal("empty counters wrote another summary"); return nil }, nil)
	s.counts[0] = math.MaxUint64
	s.add(401)
	if s.counts[0] != math.MaxUint64 || saturatedAdd(math.MaxUint64-1, 2) != math.MaxUint64 {
		t.Fatal("counter overflow wrapped")
	}
}

func TestAnonymousSummaryWriterIsSingleBoundedAndStops(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var writes, active, maximum atomic.Int32
	s := newRefusalSummary(func(ctx context.Context, e Event) error {
		n := active.Add(1)
		maximum.CompareAndSwap(0, n)
		if n != 1 {
			t.Error("summary writes overlap")
		}
		defer active.Add(-1)
		if writes.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}, nil, 10*time.Millisecond)
	t.Cleanup(s.close)
	s.add(401)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("periodic summary did not run")
	}
	for range 1000 {
		s.add(401)
	}
	if writes.Load() != 1 {
		t.Fatal("flood started another summary writer")
	}
	close(release)
	s.close()
	s.close()
	if writes.Load() != 2 || active.Load() != 0 || maximum.Load() != 1 {
		t.Fatal("shutdown lost counts or retained a writer")
	}
}

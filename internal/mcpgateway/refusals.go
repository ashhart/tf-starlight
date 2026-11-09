package mcpgateway

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"
)

type refusalSummary struct {
	mu     sync.Mutex
	counts [6]uint64
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
}

func newRefusalSummary(record func(context.Context, Event) error, log *slog.Logger, interval time.Duration) *refusalSummary {
	s := &refusalSummary{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.flush(record, log)
			case <-s.stop:
				s.flush(record, log)
				return
			}
		}
	}()
	return s
}

func (s *refusalSummary) add(status int) {
	i := 5
	switch status {
	case 401:
		i = 0
	case 403:
		i = 1
	case 405:
		i = 2
	case 429:
		i = 3
	case 503:
		i = 4
	}
	s.mu.Lock()
	s.counts[i] = saturatedAdd(s.counts[i], 1)
	s.mu.Unlock()
}

func saturatedAdd(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}

// flush runs only in the single summary worker, including its final shutdown attempt.
func (s *refusalSummary) flush(record func(context.Context, Event) error, log *slog.Logger) {
	s.mu.Lock()
	counts := s.counts
	s.counts = [6]uint64{}
	s.mu.Unlock()
	if counts == [6]uint64{} {
		return
	}
	statuses := [...]string{"401", "403", "405", "429", "503", "other"}
	e := Event{Phase: "refusals", Outcome: "refused", RefusalCounts: map[string]uint64{}}
	for i, count := range counts {
		if count != 0 {
			e.RefusalCounts[statuses[i]] = count
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := record(ctx, e); err != nil {
		s.mu.Lock()
		for i, count := range counts {
			s.counts[i] = saturatedAdd(s.counts[i], count)
		}
		s.mu.Unlock()
		if log != nil {
			log.Error("MCP anonymous refusal accounting unavailable")
		}
	}
}

func (s *refusalSummary) close() {
	s.once.Do(func() { close(s.stop) })
	<-s.done
}

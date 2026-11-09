package routing

import (
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/observations"
)

func value[T any](v T) *T { return &v }

func workTarget(id string, prompt, decode float64, now time.Time) WorkCandidate {
	return WorkCandidate{Candidate: Candidate{ID: id, Free: 4, Slots: 4}, Generation: 7, ReservedKnown: true,
		Load: observations.Load{Generation: 7, ObservedAt: now.Add(-time.Second), WindowSeconds: 1,
			PromptTokensPerSecond: value(prompt), TokensPerSecond: value(decode), Running: value(0), Waiting: value(0)}}
}

func TestShadowSeparatesPromptDecodeAndOutstandingWork(t *testing.T) {
	now := time.Now()
	candidates := []WorkCandidate{workTarget("a", 50, 2, now), workTarget("b", 100, 10, now)}
	before := slices.Clone(candidates)
	d := Evaluate(candidates, Work{Prompt: 100, Completion: 10}, "a", now)
	if d.Reason != "ready" || d.ProposedID != "b" || d.ActualSeconds != 7 || d.ProposedSeconds != 2 {
		t.Fatalf("unexpected phase effort: %+v", d)
	}
	if !reflect.DeepEqual(before, candidates) {
		t.Fatal("shadow evaluation mutated its candidate inputs")
	}
	candidates[1].ReservedTokens, candidates[1].Free = 1000, 3
	if d := Evaluate(candidates, Work{Prompt: 100, Completion: 10}, "a", now); d.Reason != "ready" || d.ProposedID != "a" {
		t.Fatalf("outstanding reservations did not affect effort: %+v", d)
	}
	candidates = []WorkCandidate{workTarget("a", 1000, 1, now), workTarget("b", 10, 100, now)}
	for _, c := range []struct {
		work Work
		want string
	}{{Work{1000, 1}, "a"}, {Work{1, 1000}, "b"}} {
		if d := Evaluate(candidates, c.work, "a", now); d.Reason != "ready" || d.ProposedID != c.want {
			t.Fatalf("phase-heavy workload %+v: %+v", c.work, d)
		}
	}
}

func TestShadowTiesIgnoreInputOrder(t *testing.T) {
	now := time.Now()
	a, b := workTarget("a", 100, 10, now), workTarget("b", 100, 10, now)
	first := Evaluate([]WorkCandidate{b, a}, Work{100, 10}, "b", now)
	second := Evaluate([]WorkCandidate{a, b}, Work{100, 10}, "b", now)
	if first != second || first.Reason != "ready" || first.ProposedID != "a" {
		t.Fatalf("unstable advisory tie: %+v, %+v", first, second)
	}
}

func TestShadowUnknownDoesNotFavorTheOtherCandidate(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name, reason string
		change       func(*WorkCandidate)
	}{
		{"stale", "unknown_observation", func(c *WorkCandidate) { c.Load.ObservedAt = now.Add(-observations.MaxAge) }},
		{"future", "unknown_observation", func(c *WorkCandidate) { c.Load.ObservedAt = now.Add(time.Microsecond) }},
		{"generation", "unknown_observation", func(c *WorkCandidate) { c.Generation++ }},
		{"legacy", "unknown_observation", func(c *WorkCandidate) { c.Load.Generation = 0 }},
		{"window", "unknown_observation", func(c *WorkCandidate) { c.Load.WindowSeconds = 0 }},
		{"missing prompt", "unknown_rate", func(c *WorkCandidate) { c.Load.PromptTokensPerSecond = nil }},
		{"idle decode", "unknown_rate", func(c *WorkCandidate) { c.Load.TokensPerSecond = value(0.0) }},
		{"nan", "unknown_rate", func(c *WorkCandidate) { c.Load.TokensPerSecond = value(math.NaN()) }},
		{"missing backlog", "unknown_backlog", func(c *WorkCandidate) { c.ReservedKnown = false }},
		{"large backlog", "unknown_backlog", func(c *WorkCandidate) { c.ReservedTokens = observations.MaxCounter + 1 }},
		{"missing running", "unknown_backlog", func(c *WorkCandidate) { c.Load.Running = nil }},
		{"untracked", "untracked_work", func(c *WorkCandidate) { c.Load.Waiting = value(1) }},
		{"invalid capacity", "invalid_capacity", func(c *WorkCandidate) { c.Free = 5 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, b := workTarget("a", 100, 10, now), workTarget("b", 1000, 100, now)
			test.change(&b)
			if d := Evaluate([]WorkCandidate{a, b}, Work{100, 10}, "a", now); d.Reason != test.reason || d.ProposedID != "" || d.ActualSeconds != 0 || d.ProposedSeconds != 0 {
				t.Fatalf("unknown candidate became a favorable comparison: %+v", d)
			}
		})
	}
}

func TestShadowBoundsAndRequiredPhases(t *testing.T) {
	now := time.Now()
	a, b := workTarget("a", 100, 10, now), workTarget("b", 100, 10, now)
	for _, work := range []Work{{}, {-1, 1}, {1, -1}, {math.MaxInt64, 1}, {1, math.MaxInt64}} {
		if d := Evaluate([]WorkCandidate{a, b}, work, "a", now); d.Reason != "invalid_work" {
			t.Fatalf("invalid work accepted: %+v", d)
		}
	}
	if d := Evaluate(make([]WorkCandidate, MaxShadowCandidates+1), Work{100, 10}, "a", now); d.Reason != "too_many_candidates" {
		t.Fatalf("unbounded comparison: %+v", d)
	}
	if d := Evaluate([]WorkCandidate{a, a}, Work{100, 10}, "a", now); d.Reason != "invalid_capacity" {
		t.Fatalf("duplicate IDs accepted: %+v", d)
	}
	if d := Evaluate([]WorkCandidate{a, b}, Work{100, 10}, "gone", now); d.Reason != "missing_actual" {
		t.Fatalf("missing actual route accepted: %+v", d)
	}
	a.Load.TokensPerSecond, b.Load.TokensPerSecond = nil, nil
	if d := Evaluate([]WorkCandidate{a, b}, Work{Prompt: 100}, "a", now); d.Reason != "ready" {
		t.Fatalf("unused decode rate was required: %+v", d)
	}
}

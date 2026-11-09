package routing

import (
	"encoding/json"
	"reflect"
	"testing"
)

func replayAffinity(target string, targets []ReplayTarget) uint64 {
	var candidates []Candidate
	for _, v := range targets {
		candidates = append(candidates, Candidate{ID: v.ID, Free: 1, Slots: 1})
	}
	for key := uint64(1); key < 4096; key++ {
		if i := pick(candidates, key, func() float64 { return 0 }); i >= 0 && candidates[i].ID == target {
			return key
		}
	}
	return 0
}

func unequalReplay() ReplayTrace {
	t := ReplayTrace{Version: 1, Seed: 9, SLO: 0.5, Starvation: 5,
		Targets: []ReplayTarget{{ID: "slow", PromptRate: 1000, DecodeRate: 1, WarmPromptRatio: 1},
			{ID: "fast", PromptRate: 1000, DecodeRate: 100, WarmPromptRatio: 1}}}
	key := replayAffinity("slow", t.Targets)
	for i := range 30 {
		t.Requests = append(t.Requests, ReplayRequest{Arrival: float64(i), Completion: 10, Affinity: key})
	}
	return t
}

func TestMatchedSyntheticReplayReportsGainAndCacheRegression(t *testing.T) {
	unequal := unequalReplay()
	hot := ReplayTrace{Version: 1, Seed: 9, SLO: 0.5, Starvation: 5,
		Targets: []ReplayTarget{{ID: "warm", PromptRate: 1000, DecodeRate: 10, WarmPromptRatio: 0.001},
			{ID: "cold-fast", PromptRate: 10000, DecodeRate: 10, WarmPromptRatio: 1}}}
	key := replayAffinity("warm", hot.Targets)
	for i := range 30 {
		hot.Requests = append(hot.Requests, ReplayRequest{Arrival: float64(i) * 12, Prompt: 10000, Completion: 1, Affinity: key, CacheKey: 7})
	}
	for name, trace := range map[string]ReplayTrace{"unequal-cold": unequal, "hot-prefix": hot} {
		t.Run(name, func(t *testing.T) {
			results, err := Replay(trace)
			if err != nil || len(results) != 2 {
				t.Fatalf("matched replay: %+v, %v", results, err)
			}
			again, err := Replay(trace)
			if err != nil || !reflect.DeepEqual(results, again) {
				t.Fatal("seeded replay is not reproducible")
			}
			for _, r := range results {
				if r.Completed != len(trace.Requests) || r.ErrorSamples != r.Completed || r.MaxQueueWait != 0 || r.Starved != 0 {
					t.Fatalf("replay accounting: %+v", r)
				}
			}
			baseline, proposed := results[0], results[1]
			if name == "unequal-cold" && (proposed.P95Latency >= baseline.P95Latency || proposed.SLOGoodput <= baseline.SLOGoodput) {
				t.Fatalf("unequal-rate control did not improve: %+v", results)
			}
			if name == "hot-prefix" && (proposed.P95Latency <= baseline.P95Latency || proposed.SLOGoodput >= baseline.SLOGoodput || baseline.CacheHits == 0) {
				t.Fatalf("missing cache credit regression was hidden: %+v", results)
			}
			raw, err := json.Marshal(results)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("synthetic-only %s %s", name, raw)
		})
	}
}

func TestStaleReplayFallsBackAndKeepsUnknownErrorUnknown(t *testing.T) {
	trace := unequalReplay()
	for i := range trace.Targets {
		trace.Targets[i].ObservationAge = 20
	}
	results, err := Replay(trace)
	if err != nil {
		t.Fatal(err)
	}
	if results[1].Fallbacks != len(trace.Requests) || results[0].P95Latency != results[1].P95Latency ||
		results[0].SLOGoodput != results[1].SLOGoodput || results[1].MedianErrorRatio != nil || results[1].ErrorSamples != 0 {
		t.Fatalf("stale estimates affected serving or invented error samples: %+v", results)
	}
}

func TestReplayRejectsMalformedOrUnboundedInput(t *testing.T) {
	for _, change := range []func(*ReplayTrace){
		func(v *ReplayTrace) { v.Version++ },
		func(v *ReplayTrace) { v.Targets[1].ID = v.Targets[0].ID },
		func(v *ReplayTrace) { v.Targets[0].DecodeRate = 0 },
		func(v *ReplayTrace) { v.Targets[0].WarmPromptRatio = 0 },
		func(v *ReplayTrace) { v.Requests[1].Arrival = -1 },
		func(v *ReplayTrace) { v.Requests[1].Prompt = -1 },
		func(v *ReplayTrace) { v.Requests = make([]ReplayRequest, 10001) },
	} {
		trace := unequalReplay()
		change(&trace)
		if _, err := Replay(trace); err == nil {
			t.Fatal("malformed replay accepted")
		}
	}
}

func TestReplayMeasuresQueueWaitAndFiniteStarvationThreshold(t *testing.T) {
	trace := ReplayTrace{Version: 1, SLO: 2, Starvation: 1,
		Targets: []ReplayTarget{{ID: "a", PromptRate: 10, DecodeRate: 10, WarmPromptRatio: 1},
			{ID: "b", PromptRate: 10, DecodeRate: 10, WarmPromptRatio: 1}}}
	for range 10 {
		trace.Requests = append(trace.Requests, ReplayRequest{Completion: 10})
	}
	results, err := Replay(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Completed != 10 || r.Duration != 5 || r.P95Latency != 5 || r.MaxQueueWait != 4 ||
			r.P95QueueWait != 4 || r.Starved != 6 || r.SLOCompletions != 4 || r.Throughput != 2 || r.SLOGoodput != 0.8 {
			t.Fatalf("queue/starvation metrics are wrong: %+v", r)
		}
	}
}

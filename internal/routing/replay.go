package routing

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/ashhart/tf-starlight/internal/observations"
)

// ReplayTarget is one synthetic single-worker replica; its explicit rates are assumptions, not device measurements.
type ReplayTarget struct {
	ID              string  `json:"id"`
	PromptRate      float64 `json:"prompt_rate"`
	DecodeRate      float64 `json:"decode_rate"`
	WarmPromptRatio float64 `json:"warm_prompt_ratio"`
	ObservationAge  float64 `json:"observation_age_seconds"`
}

// ReplayRequest holds content-free work and an opaque simulated cache key; no prompt text is accepted.
type ReplayRequest struct {
	Arrival    float64 `json:"arrival_seconds"`
	Prompt     int64   `json:"prompt_tokens"`
	Completion int64   `json:"completion_tokens"`
	Affinity   uint64  `json:"affinity"`
	CacheKey   uint64  `json:"cache_key"`
}

// ReplayTrace defines matched synthetic arms; cache state is one most recently completed key per replica.
type ReplayTrace struct {
	Version    int             `json:"version"`
	Seed       uint64          `json:"seed"`
	SLO        float64         `json:"slo_seconds"`
	Starvation float64         `json:"starvation_seconds"`
	Targets    []ReplayTarget  `json:"targets"`
	Requests   []ReplayRequest `json:"requests"`
}

type replayWorker struct {
	target ReplayTarget
	until  float64
	index  int
	cache  uint64
}

// ReplayResult describes a synthetic FIFO replay and does not establish a real-device improvement.
type ReplayResult struct {
	Policy           string   `json:"policy"`
	Completed        int      `json:"completed"`
	SLOCompletions   int      `json:"slo_completions"`
	Starved          int      `json:"starved"`
	Fallbacks        int      `json:"fallbacks"`
	Comparisons      int      `json:"comparisons"`
	Disagreements    int      `json:"disagreements"`
	CacheHits        int      `json:"simulated_cache_hits"`
	Duration         float64  `json:"duration_seconds"`
	Throughput       float64  `json:"throughput_requests_per_second"`
	SLOGoodput       float64  `json:"slo_goodput_requests_per_second"`
	P95Latency       float64  `json:"p95_latency_seconds"`
	P95QueueWait     float64  `json:"p95_queue_wait_seconds"`
	MaxQueueWait     float64  `json:"max_queue_wait_seconds"`
	ErrorSamples     int      `json:"error_samples"`
	MedianErrorRatio *float64 `json:"median_observed_estimated_ratio"`
}

// Replay compares the current picker with effort under declared synthetic rates, cache behavior and identical arrivals.
func Replay(trace ReplayTrace) ([]ReplayResult, error) {
	if err := validateReplay(trace); err != nil {
		return nil, err
	}
	var out []ReplayResult
	for _, policy := range []string{"affinity_headroom", "effort"} {
		r, err := replayArm(trace, policy)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func replayArm(trace ReplayTrace, policy string) (ReplayResult, error) {
	r := ReplayResult{Policy: policy}
	workers := make([]replayWorker, len(trace.Targets))
	for i, target := range trace.Targets {
		workers[i] = replayWorker{target: target, index: -1}
	}
	rng := rand.New(rand.NewPCG(trace.Seed, trace.Seed^0x85ebca6b))
	var queue []int
	var latencySamples, waitSamples, ratios []float64
	now, arrived, served := 0.0, 0, 0
	for r.Completed < len(trace.Requests) {
		for i := range workers {
			w := &workers[i]
			if w.index >= 0 && w.until <= now {
				request := trace.Requests[w.index]
				latency := w.until - request.Arrival
				latencySamples = append(latencySamples, latency)
				if latency <= trace.SLO {
					r.SLOCompletions++
				}
				w.cache, w.index = request.CacheKey, -1
				r.Completed++
			}
		}
		for arrived < len(trace.Requests) && trace.Requests[arrived].Arrival <= now {
			queue = append(queue, arrived)
			arrived++
		}
		for served < len(queue) {
			request := trace.Requests[queue[served]]
			candidates, plain := replayCandidates(workers, now)
			chosen := pick(plain, request.Affinity, rng.Float64)
			if chosen < 0 {
				break
			}
			work := Work{Prompt: request.Prompt, Completion: request.Completion}
			decision := Evaluate(candidates, work, workers[chosen].target.ID, replayTime(now))
			if decision.Reason == "ready" {
				r.Comparisons++
				if decision.ActualID != decision.ProposedID {
					r.Disagreements++
				}
			}
			if policy == "effort" {
				if decision.Reason == "ready" || decision.Reason == "no_alternative" {
					for i := range workers {
						if workers[i].target.ID == decision.ProposedID {
							chosen = i
						}
					}
				} else {
					r.Fallbacks++
				}
			}
			w := &workers[chosen]
			prompt := float64(request.Prompt) / w.target.PromptRate
			if request.CacheKey != 0 && request.CacheKey == w.cache {
				prompt *= w.target.WarmPromptRatio
				r.CacheHits++
			}
			duration := prompt + float64(request.Completion)/w.target.DecodeRate
			if now+duration <= now || !finiteReplay(now+duration) {
				return r, errors.New("replay service duration exceeds the simulation's precision or bounds")
			}
			wait := now - request.Arrival
			waitSamples = append(waitSamples, wait)
			r.MaxQueueWait = max(r.MaxQueueWait, wait)
			if wait > trace.Starvation {
				r.Starved++
			}
			if score, reason := effort(candidates[chosen], work, replayTime(now)); reason == "" {
				ratios = append(ratios, duration/score)
			}
			w.until, w.index = now+duration, queue[served]
			served++
		}
		if r.Completed == len(trace.Requests) {
			break
		}
		next := math.Inf(1)
		if arrived < len(trace.Requests) {
			next = trace.Requests[arrived].Arrival
		}
		for _, w := range workers {
			if w.index >= 0 {
				next = min(next, w.until)
			}
		}
		if next <= now || math.IsInf(next, 0) {
			return r, errors.New("replay cannot advance")
		}
		now = next
	}
	r.Duration = now - trace.Requests[0].Arrival
	r.Throughput = float64(r.Completed) / r.Duration
	r.SLOGoodput = float64(r.SLOCompletions) / r.Duration
	r.P95Latency, r.P95QueueWait = percentile(latencySamples, 0.95), percentile(waitSamples, 0.95)
	r.ErrorSamples = len(ratios)
	if len(ratios) > 0 {
		median := percentile(ratios, 0.5)
		r.MedianErrorRatio = &median
	}
	return r, nil
}

func replayCandidates(workers []replayWorker, at float64) ([]WorkCandidate, []Candidate) {
	var out []WorkCandidate
	var plain []Candidate
	for _, w := range workers {
		free := 1
		if w.index >= 0 {
			free = 0
		}
		prompt, decode, running, waiting := w.target.PromptRate, w.target.DecodeRate, 1-free, 0
		c := Candidate{ID: w.target.ID, Slots: 1, Free: free}
		out = append(out, WorkCandidate{Candidate: c, Generation: 1, ReservedKnown: true,
			Load: observations.Load{Generation: 1, ObservedAt: replayTime(at - w.target.ObservationAge), WindowSeconds: 1,
				PromptTokensPerSecond: &prompt, TokensPerSecond: &decode, Running: &running, Waiting: &waiting}})
		plain = append(plain, c)
	}
	return out, plain
}

func replayTime(seconds float64) time.Time {
	return time.Unix(1_700_000_000, 0).Add(time.Duration(seconds * 1e9))
}

func finiteReplay(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 365*86400
}

func validateReplay(t ReplayTrace) error {
	if t.Version != 1 || len(t.Targets) < 2 || len(t.Targets) > MaxShadowCandidates || len(t.Requests) < 1 || len(t.Requests) > 10000 ||
		!finiteReplay(t.SLO) || t.SLO <= 0 || !finiteReplay(t.Starvation) || t.Starvation <= 0 {
		return errors.New("replay requires version1, 2..128 single-worker targets, 1..10000 requests and positive bounded thresholds")
	}
	ids := map[string]bool{}
	for _, target := range t.Targets {
		if target.ID == "" || len(target.ID) > 64 || ids[target.ID] || !validReplayRate(target.PromptRate) || !validReplayRate(target.DecodeRate) ||
			math.IsNaN(target.WarmPromptRatio) || target.WarmPromptRatio <= 0 || target.WarmPromptRatio > 1 || !finiteReplay(target.ObservationAge) {
			return errors.New("replay target IDs must be unique and rates, warm ratios and observation ages must be valid")
		}
		ids[target.ID] = true
	}
	last := 0.0
	for i, request := range t.Requests {
		if !finiteReplay(request.Arrival) || request.Arrival < last || request.Prompt < 0 || request.Completion < 0 ||
			request.Prompt > observations.MaxCounter-request.Completion || request.Prompt+request.Completion == 0 {
			return fmt.Errorf("replay request %d has invalid arrival order or work", i)
		}
		last = request.Arrival
	}
	return nil
}

func validReplayRate(v float64) bool {
	return v > 0 && v <= observations.MaxRate && !math.IsNaN(v) && !math.IsInf(v, 0)
}

package engine

import (
	"math"

	"github.com/ashhart/tf-starlight/internal/control/wire"
)

func legacySum(s *sample, family string) (float64, bool) {
	total := 0.0
	known := false
	for _, v := range s.fam[family] {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		total += v
		known = true
	}
	return total, known
}

func legacyNonnegative(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

func legacyCount(v float64) int { return int(min(legacyNonnegative(v), math.MaxInt32)) }

func legacyRate(prev, cur *sample, families ...string) float64 {
	if prev == nil || prev.base != cur.base || prev.epoch != cur.epoch {
		return 0
	}
	a, okA := legacySum(prev, families[0])
	b, okB := legacySum(cur, families[0])
	for _, family := range families[1:] {
		av, _ := legacySum(prev, family)
		bv, _ := legacySum(cur, family)
		a += av
		b += bv
	}
	seconds := cur.at.Sub(prev.at).Seconds()
	if !okA || !okB || b < a || seconds <= 0 {
		return 0
	}
	return legacyNonnegative((b - a) / seconds)
}

// legacyStats retains the original report's fallback and clamping for nodes talking to older servers.
func legacyStats(id string, prev, cur *sample, inFlight int) wire.ReplicaStats {
	running, ok := legacySum(cur, famRunning)
	if !ok {
		running = float64(inFlight)
	}
	waiting, _ := legacySum(cur, famWaiting)
	kv := 0.0
	for _, value := range cur.fam[famKV] {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		kv = max(kv, value)
	}
	return wire.ReplicaStats{ReplicaID: id, TokensPerSecond: legacyRate(prev, cur, famGenTotal, famGenRunning),
		PromptTokensPerSecond: legacyRate(prev, cur, famPrompt), Running: legacyCount(running), Waiting: legacyCount(waiting),
		KVUsage: min(legacyNonnegative(kv), 1)}
}

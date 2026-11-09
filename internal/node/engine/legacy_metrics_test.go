package engine

import (
	"testing"
	"time"
)

func TestLegacyReportsKeepInflightFallbackAndKVClamp(t *testing.T) {
	cur := &sample{at: time.Now(), fam: map[string][]float64{famKV: {1.5}}}
	legacy := legacyStats("replica", nil, cur, 3)
	if legacy.Running != 3 || legacy.KVUsage != 1 {
		t.Fatalf("legacy fallback/clamp lost: %+v", legacy)
	}
	if measuredCount(cur, famRunning) != nil {
		t.Fatal("legacy fallback leaked into a measured running count")
	}
	if _, ok := cur.max(famKV); ok {
		t.Fatal("legacy clamp leaked into a measured KV ratio")
	}
}

func TestLegacyRatesRetainLongIntervalsWithoutQualifyingThem(t *testing.T) {
	prev := &sample{at: time.Now(), base: "engine", fam: map[string][]float64{famGenTotal: {100}, famPrompt: {100}}}
	cur := &sample{at: prev.at.Add(20 * time.Second), base: "engine", fam: map[string][]float64{famGenTotal: {200}, famPrompt: {300}}}
	legacy := legacyStats("replica", prev, cur, 0)
	if legacy.TokensPerSecond != 5 || legacy.PromptTokensPerSecond != 10 {
		t.Fatalf("legacy rates changed: %+v", legacy)
	}
	if measuredRate(prev, cur, prompted) != nil {
		t.Fatal("expired rate window became a qualified observation")
	}
}

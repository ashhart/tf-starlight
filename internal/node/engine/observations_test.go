package engine

import (
	"strings"
	"testing"
	"time"
)

func TestRateDistinguishesIdleFromUnknownAndProcessChanges(t *testing.T) {
	now := time.Now()
	makeSample := func(at time.Time, count float64) *sample {
		return &sample{at: at, base: "engine", epoch: 1, fam: map[string][]float64{famPrompt: {count}}}
	}
	a, b := makeSample(now, 100), makeSample(now.Add(5*time.Second), 100)
	if v := measuredRate(a, b, prompted); v == nil || *v != 0 {
		t.Fatal("measured idle became unknown")
	}
	for name, cur := range map[string]*sample{
		"reset": makeSample(b.at, 1), "duplicate": makeSample(now, 100), "expired window": makeSample(now.Add(16*time.Second), 100),
		"absent": {at: b.at, base: "engine", epoch: 1}, "restart": {at: b.at, base: "engine", epoch: 2, fam: b.fam},
	} {
		if v := measuredRate(a, cur, prompted); v != nil {
			t.Fatalf("%s produced rate %v", name, *v)
		}
	}
	if measuredRate(nil, b, prompted) != nil {
		t.Fatal("first sample produced a rate")
	}
}

func TestMetricFamiliesRejectPartialInvalidAndChangedSeries(t *testing.T) {
	for _, text := range []string{
		"tensorfold:prompt_tokens_total{lane=\"a\"} 1\ntensorfold:prompt_tokens_total{lane=\"b\"} NaN\n",
		"tensorfold:prompt_tokens_total 1\ntensorfold:prompt_tokens_total bad\n",
		"tensorfold:prompt_tokens_total 1\ntensorfold:prompt_tokens_total 1\n",
		"tensorfold:prompt_tokens_total -1\n",
		"tensorfold:prompt_tokens_total 9007199254740992\n",
		"tensorfold:prompt_tokens_total 1.5\n",
		"tensorfold:prompt_tokens_total{lane=\"a\"} 0.5\ntensorfold:prompt_tokens_total{lane=\"b\"} 0.5\n",
	} {
		if _, ok := prompted(parseMetrics(strings.NewReader(text))); ok {
			t.Fatalf("invalid family accepted: %q", text)
		}
	}
	a := parseMetrics(strings.NewReader("tensorfold:prompt_tokens_total{lane=\"a\"} 100\n"))
	b := parseMetrics(strings.NewReader("tensorfold:prompt_tokens_total{lane=\"b\"} 200\n"))
	a.at = time.Now()
	b.at = a.at.Add(time.Second)
	if measuredRate(a, b, prompted) != nil {
		t.Fatal("replacement series produced a rate")
	}
	if measuredCount(parseMetrics(strings.NewReader("tensorfold:requests_waiting 1.5\n")), famWaiting) != nil {
		t.Fatal("fractional request count accepted")
	}
	if _, ok := produced(&sample{fam: map[string][]float64{famGenTotal: {10}}}); ok {
		t.Fatal("missing running-token counter accepted")
	}
}

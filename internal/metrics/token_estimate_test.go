package metrics

import (
	"strings"
	"testing"

	"github.com/ashhart/tf-starlight/internal/observations"
	"github.com/ashhart/tf-starlight/internal/routing"
)

func TestTokenEstimateErrorRecordsUnderOverAndExplicitZero(t *testing.T) {
	s := New()
	s.ObserveTokenEstimate("model", routing.Work{Prompt: 8, Completion: 262144}, routing.Work{Prompt: 4096, Completion: 1}, true, 200, "ok")
	s.ObserveTokenEstimate("model", routing.Work{}, routing.Work{}, true, 200, "ok")
	want := map[string]struct {
		count uint64
		sum   float64
	}{
		"prompt/under": {1, 4088}, "completion/over": {1, 262143}, "prompt/equal": {1, 0}, "completion/equal": {1, 0},
	}
	families, err := s.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "starlight_routing_shadow_token_error_tokens" {
			continue
		}
		for _, m := range f.Metric {
			labels := map[string]string{}
			for _, l := range m.Label {
				labels[l.GetName()] = l.GetValue()
			}
			key := labels["phase"] + "/" + labels["direction"]
			w, ok := want[key]
			if !ok || len(labels) != 3 || labels["model"] != "model" || m.GetHistogram().GetSampleCount() != w.count || m.GetHistogram().GetSampleSum() != w.sum {
				t.Fatalf("unexpected calibration observation: %v %v", labels, m.GetHistogram())
			}
			delete(want, key)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing error observations: %v", want)
	}
}

func TestTokenEstimateErrorOmitsUnknownFailedAndInvalidCounts(t *testing.T) {
	s := New()
	w := routing.Work{Prompt: 9, Completion: 32}
	s.ObserveTokenEstimate("model", w, w, false, 200, "ok")
	s.ObserveTokenEstimate("model", w, w, true, 503, "ok")
	s.ObserveTokenEstimate("model", w, w, true, 200, "cancelled")
	for _, bad := range []int64{-1, observations.MaxCounter + 1} {
		for i := 0; i < 4; i++ {
			v := []int64{9, 32, 8, 2}
			v[i] = bad
			s.ObserveTokenEstimate("model", routing.Work{Prompt: v[0], Completion: v[1]}, routing.Work{Prompt: v[2], Completion: v[3]}, true, 200, "ok")
		}
	}
	families, err := s.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if strings.Contains(f.GetName(), "token_error_tokens") {
			t.Fatal("invalid work became a numeric calibration sample")
		}
	}
}

package metrics

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/routing"
)

func TestRoutingShadowMetricsSeparateScoresFromObservedError(t *testing.T) {
	s := New()
	d := routing.Decision{Reason: "ready", ActualID: "private-actual", ProposedID: "private-proposal", ActualSeconds: 2, ProposedSeconds: 1}
	s.ObserveRouting("model", d, 4*time.Second, true, 200, "ok")
	s.ObserveRouting("model", d, 4*time.Second, false, 200, "ok")
	s.ObserveRouting("model", d, 4*time.Second, true, 502, "upstream_unreachable")
	if n := routingDecisions(t, s); n != 3 {
		t.Fatalf("shadow decisions=%v", n)
	}
	metrics, err := s.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var errorSamples uint64
	for _, family := range metrics {
		if family.GetName() == "starlight_routing_shadow_observed_estimated_ratio" {
			for _, m := range family.Metric {
				errorSamples += m.GetHistogram().GetSampleCount()
				if m.GetHistogram().GetSampleSum() != 2 {
					t.Fatalf("prediction error did not use actual elapsed/actual score: %v", m.GetHistogram())
				}
			}
		}
		for _, m := range family.Metric {
			for _, label := range m.Label {
				if strings.Contains(label.GetValue(), "private-") {
					t.Fatal("a target ID became a metric label")
				}
			}
		}
	}
	if errorSamples != 1 {
		t.Fatalf("unobserved/failed work became qualifying error samples: %d", errorSamples)
	}
}

func TestRoutingShadowRejectsInvalidValuesAndLabels(t *testing.T) {
	s := New()
	for _, score := range []float64{math.NaN(), math.Inf(1), -1, 0, 100000} {
		d := routing.Decision{Reason: "ready", ActualID: "a", ProposedID: "b", ActualSeconds: score, ProposedSeconds: 1}
		s.ObserveRouting("model", d, time.Second, true, 200, "ok")
	}
	s.ObserveRouting("model", routing.Decision{Reason: "caller-controlled"}, time.Second, true, 200, "ok")
	if n := routingDecisions(t, s); n != 6 {
		t.Fatalf("invalid comparisons=%v", n)
	}
	metrics, err := s.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range metrics {
		if f.GetName() == "starlight_routing_shadow_effort_seconds" || f.GetName() == "starlight_routing_shadow_observed_estimated_ratio" {
			t.Fatalf("invalid comparisons produced numeric samples: %s", f.GetName())
		}
	}
}

func routingDecisions(t *testing.T, s *Set) float64 {
	t.Helper()
	families, err := s.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var count float64
	for _, family := range families {
		if family.GetName() == "starlight_routing_shadow_decisions_total" {
			for _, metric := range family.Metric {
				count += metric.GetCounter().GetValue()
			}
		}
	}
	return count
}

package metrics

import (
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ashhart/tf-starlight/internal/routing"
)

type routeMetrics struct {
	decisions   *prometheus.CounterVec
	scores      *prometheus.HistogramVec
	errors      *prometheus.HistogramVec
	outcomes    *prometheus.CounterVec
	tokenErrors *prometheus.HistogramVec
}

func newRouteMetrics(r *prometheus.Registry) *routeMetrics {
	s := &routeMetrics{
		tokenErrors: newTokenEstimateMetric(),
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "starlight_routing_shadow_decisions_total",
			Help: "Advisory comparisons for actual committed leases; live routing is unchanged."}, []string{"model", "reason", "agreement"}),
		scores: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "starlight_routing_shadow_effort_seconds",
			Help: "Heuristic phase-work and reservation effort, not an SLO bound.", Buckets: latency}, []string{"model", "choice"}),
		errors: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "starlight_routing_shadow_observed_estimated_ratio",
			Help:    "Actual admit-to-finished-relay seconds divided by estimated effort for a successful observed actual route.",
			Buckets: []float64{0.01, 0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 64, 256}}, []string{"model"}),
		outcomes: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "starlight_routing_shadow_results_total",
			Help: "Which actual-route completions can qualify prediction-error observations."}, []string{"model", "result"}),
	}
	r.MustRegister(s.decisions, s.scores, s.errors, s.outcomes, s.tokenErrors)
	return s
}

// ObserveRouting keeps target IDs private and records proposed effort separately from actual-route prediction error.
func (s *Set) ObserveRouting(model string, d routing.Decision, elapsed time.Duration, observed bool, status int, outcome string) {
	reason := routingReason(d.Reason)
	if reason == "ready" && (d.ActualID == "" || d.ProposedID == "" || !validEffort(d.ActualSeconds) || !validEffort(d.ProposedSeconds)) {
		reason = "invalid"
	}
	agreement := "unavailable"
	if reason == "ready" {
		agreement = "different"
		if d.ActualID == d.ProposedID {
			agreement = "same"
		}
	}
	s.shadow.decisions.WithLabelValues(model, reason, agreement).Inc()
	if reason != "ready" {
		return
	}
	s.shadow.scores.WithLabelValues(model, "actual").Observe(d.ActualSeconds)
	s.shadow.scores.WithLabelValues(model, "proposed").Observe(d.ProposedSeconds)
	result := "observed"
	switch {
	case status < 200 || status >= 300 || outcome != "ok":
		result = "failed"
	case !observed || elapsed <= 0 || d.ActualSeconds <= 0:
		result = "unobserved"
	default:
		ratio := elapsed.Seconds() / d.ActualSeconds
		if math.IsNaN(ratio) || math.IsInf(ratio, 0) {
			result = "invalid"
		} else {
			s.shadow.errors.WithLabelValues(model).Observe(ratio)
		}
	}
	s.shadow.outcomes.WithLabelValues(model, result).Inc()
}

func validEffort(v float64) bool {
	return v > 0 && v <= (24*time.Hour).Seconds() && !math.IsNaN(v) && !math.IsInf(v, 0)
}

func routingReason(reason string) string {
	switch reason {
	case "ready", "no_alternative", "missing_actual", "too_many_candidates", "invalid_work", "invalid_capacity",
		"unknown_observation", "unknown_backlog", "untracked_work", "unknown_rate", "invalid_score", "cloud_target":
		return reason
	default:
		return "invalid"
	}
}

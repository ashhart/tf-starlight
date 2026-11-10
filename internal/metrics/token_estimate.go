package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ashhart/tf-starlight/internal/observations"
	"github.com/ashhart/tf-starlight/internal/routing"
)

func newTokenEstimateMetric() *prometheus.HistogramVec {
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "starlight_routing_shadow_token_error_tokens",
		Help:    "Absolute estimated-versus-reported token difference on successful observed local generation; not memory demand.",
		Buckets: []float64{0, 1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216},
	}, []string{"model", "phase", "direction"})
}

// ObserveTokenEstimate records complete token-count observations without changing reservations or learning a policy.
func (s *Set) ObserveTokenEstimate(model string, estimate, actual routing.Work, observed bool, status int, outcome string) {
	if !observed || status < 200 || status >= 300 || outcome != "ok" {
		return
	}
	for _, n := range []int64{estimate.Prompt, estimate.Completion, actual.Prompt, actual.Completion} {
		if n < 0 || n > observations.MaxCounter {
			return
		}
	}
	for _, phase := range []struct {
		name              string
		estimated, actual int64
	}{
		{"prompt", estimate.Prompt, actual.Prompt}, {"completion", estimate.Completion, actual.Completion},
	} {
		difference := phase.estimated - phase.actual
		direction := "over"
		if difference == 0 {
			direction = "equal"
		} else if difference < 0 {
			direction, difference = "under", -difference
		}
		s.shadow.tokenErrors.WithLabelValues(model, phase.name, direction).Observe(float64(difference))
	}
}

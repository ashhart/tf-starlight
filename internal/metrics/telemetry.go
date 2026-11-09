package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ashhart/tf-starlight/internal/telemetry"
)

// RegisterTelemetry exposes fixed exporter-health counters without identity, endpoint or error-payload labels.
func (s *Set) RegisterTelemetry(r *telemetry.Recorder) {
	counters := []struct {
		name string
		read func(telemetry.Stats) uint64
	}{
		{"enqueued", func(s telemetry.Stats) uint64 { return s.Enqueued }},
		{"sent", func(s telemetry.Stats) uint64 { return s.Sent }},
		{"queue_full", func(s telemetry.Stats) uint64 { return s.QueueFull }},
		{"failed", func(s telemetry.Stats) uint64 { return s.Failed }},
		{"rejected", func(s telemetry.Stats) uint64 { return s.Rejected }},
		{"shutdown", func(s telemetry.Stats) uint64 { return s.Shutdown }},
		{"invalid", func(s telemetry.Stats) uint64 { return s.Invalid }},
		{"requests", func(s telemetry.Stats) uint64 { return s.Requests }},
		{"retries", func(s telemetry.Stats) uint64 { return s.Retries }},
		{"partial", func(s telemetry.Stats) uint64 { return s.Partial }},
		{"policy", func(s telemetry.Stats) uint64 { return s.Policy }},
	}
	for _, counter := range counters {
		s.Registry.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "starlight_phase_telemetry_" + counter.name + "_total", Help: "Fixed phase telemetry exporter counter."},
			func() float64 { return float64(counter.read(r.Stats())) }))
	}
	s.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "starlight_phase_telemetry_queue",
		Help: "Currently queued fixed-shape span records."}, func() float64 { return float64(r.Stats().QueueLength) }))
	s.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "starlight_phase_telemetry_success_timestamp_seconds",
		Help: "Last collector response accepting at least one span; zero before any acceptance."}, func() float64 { return float64(r.Stats().LastSuccess) }))
}

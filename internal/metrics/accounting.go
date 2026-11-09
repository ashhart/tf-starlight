package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ashhart/tf-starlight/internal/accounting"
)

// RegisterAccountingMaintenance returns a fixed-label observer for one control-role instance.
func (s *Set) RegisterAccountingMaintenance() func(accounting.MaintenanceEvent) {
	steps := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "starlight_accounting_maintenance_total",
		Help: "Completed maintenance steps by fixed stage and result."}, []string{"stage", "result"})
	errors := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "starlight_accounting_maintenance_errors_total",
		Help: "Maintenance failures with bounded error classes, without error payloads."}, []string{"stage", "class"})
	counts := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "starlight_accounting_maintenance_rows_total",
		Help: "Committed maintenance effects by stage."}, []string{"stage"})
	failed := prometheus.NewCounter(prometheus.CounterOpts{Name: "starlight_accounting_audit_failed_attempts_total",
		Help: "Per-lease audit attempts that failed before linkage; durable work remains pending."})
	durations := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "starlight_accounting_maintenance_seconds",
		Help: "Maintenance step duration.", Buckets: []float64{0.001, 0.01, 0.1, 0.5, 1, 2, 5}}, []string{"stage"})
	backlog := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "starlight_accounting_backlog",
		Help: "Bounded maintenance backlog counts; capped counts are lower bounds."}, []string{"kind"})
	capped := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "starlight_accounting_backlog_capped",
		Help: "1 means the count is a lower bound at 100000."}, []string{"kind"})
	oldest := prometheus.NewGauge(prometheus.GaugeOpts{Name: "starlight_accounting_oldest_pending_seconds",
		Help: "Age of the oldest pending accounting audit event, including deferred retries."})
	available := prometheus.NewGauge(prometheus.GaugeOpts{Name: "starlight_accounting_backlog_available",
		Help: "1 when the last bounded backlog read succeeded; failed reads preserve earlier gauge values."})
	observedAt := prometheus.NewGauge(prometheus.GaugeOpts{Name: "starlight_accounting_backlog_observed_timestamp_seconds",
		Help: "Time of the last successful backlog snapshot."})
	s.Registry.MustRegister(steps, errors, counts, failed, durations, backlog, capped, oldest, available, observedAt)
	return func(e accounting.MaintenanceEvent) {
		stage := e.Stage
		switch stage {
		case "reap", "drain", "prune", "backlog":
		default:
			stage = "invalid"
		}
		result := "success"
		if e.Limited {
			result = "budget_exhausted"
		}
		if e.Err != nil {
			result = "failed"
			errors.WithLabelValues(stage, accounting.ErrorClass(e.Err)).Inc()
		}
		steps.WithLabelValues(stage, result).Inc()
		counts.WithLabelValues(stage).Add(float64(max(0, e.Count)))
		failed.Add(float64(max(0, e.Failed)))
		durations.WithLabelValues(stage).Observe(max(0, e.Elapsed.Seconds()))
		if stage == "backlog" {
			available.Set(0)
			if e.Err == nil && e.Backlog != nil {
				b := e.Backlog
				backlog.WithLabelValues("pending_audit").Set(float64(b.Pending))
				backlog.WithLabelValues("expired_receipts").Set(float64(b.Expired))
				capped.WithLabelValues("pending_audit").Set(truth(b.PendingCapped))
				capped.WithLabelValues("expired_receipts").Set(truth(b.ExpiredCapped))
				oldest.Set(b.OldestPending)
				available.Set(1)
				observedAt.Set(float64(time.Now().Unix()))
			}
		}
	}
}

func truth(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

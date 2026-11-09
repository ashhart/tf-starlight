package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/ashhart/tf-starlight/internal/accounting"
)

func (h *housekeeper) maintainAccounting(ctx context.Context) {
	start := time.Now()
	auditCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	drained, err := accounting.DrainUntil(auditCtx, h.pool)
	cancel()
	h.reportAccounting(accounting.MaintenanceEvent{Stage: "drain", Count: drained.Appended, Failed: drained.Failed,
		Batches: drained.Batches, Limited: drained.BudgetExhausted, Elapsed: time.Since(start), Err: err})
	start = time.Now()
	pruneCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	pruned, err := accounting.PruneUntil(pruneCtx, h.pool)
	cancel()
	h.reportAccounting(accounting.MaintenanceEvent{Stage: "prune", Count: pruned.Deleted, Batches: pruned.Batches,
		Limited: pruned.BudgetExhausted, Elapsed: time.Since(start), Err: err})
	start = time.Now()
	backlogCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	backlog, err := accounting.ReadBacklog(backlogCtx, h.pool)
	cancel()
	event := accounting.MaintenanceEvent{Stage: "backlog", Elapsed: time.Since(start), Err: err}
	if err == nil {
		event.Backlog = &backlog
	}
	h.reportAccounting(event)
}

func (h *housekeeper) reportAccounting(e accounting.MaintenanceEvent) {
	if h.accounting != nil {
		h.accounting(e)
	}
	log := h.log
	if log == nil {
		log = slog.Default()
	}
	if e.Err != nil {
		log.Warn("accounting maintenance failed", "stage", e.Stage, "error_class", accounting.ErrorClass(e.Err),
			"completed", e.Count, "failed_leases", e.Failed)
	}
	if e.Limited {
		log.Warn("accounting maintenance budget exhausted", "stage", e.Stage, "completed", e.Count, "batches", e.Batches)
	}
}

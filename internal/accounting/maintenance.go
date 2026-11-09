package accounting

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DrainResult counts committed appends, failed leases and bounded selections separately from deadline exhaustion.
type DrainResult struct {
	Appended, Failed, Selected, Batches int
	BudgetExhausted                     bool
}

// PruneResult counts committed deletions across small transactions until drained or the caller's deadline expires.
type PruneResult struct {
	Deleted, Batches int
	BudgetExhausted  bool
}

// DrainUntil handles multiple batches; one poisoned lease is rescheduled instead of stopping unrelated leases.
func DrainUntil(ctx context.Context, pool *pgxpool.Pool) (DrainResult, error) {
	var total DrainResult
	if _, ok := ctx.Deadline(); !ok {
		return total, errors.New("accounting maintenance requires a deadline")
	}
	var first error
	for ctx.Err() == nil {
		r, err := drainBatch(ctx, pool, 100)
		total.Appended, total.Failed = total.Appended+r.Appended, total.Failed+r.Failed
		total.Selected, total.Batches = total.Selected+r.Selected, total.Batches+1
		if err != nil && first == nil && ctx.Err() == nil {
			first = err
		}
		if r.Selected < 100 || r.Appended == 0 && r.Failed == 0 || ctx.Err() != nil {
			break
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		total.BudgetExhausted = true
	} else if ctx.Err() != nil && first == nil {
		first = ctx.Err()
	}
	return total, first
}

// PruneUntil removes batches of at most 1,000 with no fixed per-pass row ceiling, bounded by a required deadline.
func PruneUntil(ctx context.Context, pool *pgxpool.Pool) (PruneResult, error) {
	var result PruneResult
	if _, ok := ctx.Deadline(); !ok {
		return result, errors.New("accounting maintenance requires a deadline")
	}
	for ctx.Err() == nil {
		n, err := Prune(ctx, pool)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				result.BudgetExhausted = true
				return result, nil
			}
			return result, err
		}
		result.Deleted, result.Batches = result.Deleted+n, result.Batches+1
		if n < 1000 {
			return result, nil
		}
	}
	result.BudgetExhausted = errors.Is(ctx.Err(), context.DeadlineExceeded)
	if result.BudgetExhausted {
		return result, nil
	}
	return result, ctx.Err()
}

// ErrorClass is bounded diagnostic metadata; it never exports a connection URL, SQL payload or error message.
func ErrorClass(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23514", "23503", "23505":
			return "integrity"
		case "55P03":
			return "lock_timeout"
		case "40P01", "40001":
			return "transaction"
		case "57014":
			return "query_cancelled"
		default:
			return "postgres"
		}
	}
	if err == nil {
		return ""
	}
	return "other"
}

// MaintenanceEvent carries fixed-stage health and counts from one leader maintenance step.
type MaintenanceEvent struct {
	Stage   string
	Count   int
	Failed  int
	Batches int
	Limited bool
	Elapsed time.Duration
	Err     error
	Backlog *Backlog
}

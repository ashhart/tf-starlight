package accounting

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// BacklogCap bounds each sampled count; a capped count is a lower bound, never a claim that larger work is absent.
const BacklogCap = 100_000

// Backlog is a bounded snapshot of pending audit work and currently eligible receipt cleanup.
type Backlog struct {
	Pending, Expired int64
	PendingCapped    bool
	ExpiredCapped    bool
	OldestPending    float64
}

// ReadBacklog returns capped counts and the oldest pending age, independently of deferred retry eligibility.
func ReadBacklog(ctx context.Context, pool *pgxpool.Pool) (Backlog, error) {
	var r Backlog
	err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM (SELECT 1 FROM accounting_audit WHERE audit_seq IS NULL LIMIT 100001) q),
		coalesce((SELECT greatest(0,extract(epoch FROM clock_timestamp()-created_at))::double precision
		FROM accounting_audit WHERE audit_seq IS NULL ORDER BY created_at LIMIT 1),0),
		(SELECT count(*) FROM (SELECT 1 FROM terminal_leases t WHERE t.late_until<now()
		AND NOT EXISTS (SELECT 1 FROM accounting_audit a WHERE a.lease_id=t.lease_id AND a.audit_seq IS NULL)
		AND NOT EXISTS (SELECT 1 FROM late_usage l WHERE l.lease_id=t.lease_id AND l.created_at>=now()-interval '90 days')
		LIMIT 100001) q)`).Scan(&r.Pending, &r.OldestPending, &r.Expired)
	if err != nil {
		return Backlog{}, err
	}
	r.PendingCapped, r.ExpiredCapped = r.Pending > BacklogCap, r.Expired > BacklogCap
	r.Pending, r.Expired = min(r.Pending, BacklogCap), min(r.Expired, BacklogCap)
	return r, nil
}

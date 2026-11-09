package accounting

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ashhart/tf-starlight/internal/db"
)

// Prune removes at most 1,000 expired receipts, retaining every pending audit event and 90 days of late reports.
func Prune(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	count := 0
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT t.lease_id::text FROM terminal_leases t
			WHERE t.late_until < now()
			AND NOT EXISTS (SELECT 1 FROM accounting_audit a WHERE a.lease_id=t.lease_id AND a.audit_seq IS NULL)
			AND NOT EXISTS (SELECT 1 FROM late_usage l WHERE l.lease_id=t.lease_id AND l.created_at >= now()-interval '90 days')
			ORDER BY t.late_until,t.lease_id LIMIT 1000 FOR UPDATE OF t SKIP LOCKED`)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		if err := tx.QueryRow(ctx, `SELECT ARRAY(SELECT t.lease_id::text FROM terminal_leases t
			WHERE t.lease_id=ANY($1::uuid[]) AND t.late_until < now()
			AND NOT EXISTS (SELECT 1 FROM accounting_audit a WHERE a.lease_id=t.lease_id AND a.audit_seq IS NULL)
			AND NOT EXISTS (SELECT 1 FROM late_usage l WHERE l.lease_id=t.lease_id AND l.created_at >= now()-interval '90 days'))`, ids).
			Scan(&ids); err != nil {
			return err
		}
		for _, table := range []string{"accounting_audit", "late_usage", "terminal_leases"} {
			if _, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE lease_id=ANY($1::uuid[])", ids); err != nil {
				return err
			}
		}
		count = len(ids)
		return nil
	})
	return count, err
}

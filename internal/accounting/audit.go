// Package accounting retains terminal charges and late observations without changing settled budgets.
package accounting

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ashhart/tf-starlight/internal/audit"
	"github.com/ashhart/tf-starlight/internal/db"
)

// Drain attempts at most limit pending events, backing off failing leases while continuing with healthy work.
func Drain(ctx context.Context, pool *pgxpool.Pool, limit int) (int, error) {
	r, err := drainBatch(ctx, pool, limit)
	return r.Appended, err
}

func drainBatch(ctx context.Context, pool *pgxpool.Pool, limit int) (DrainResult, error) {
	var result DrainResult
	if limit < 1 || limit > 100 {
		return result, errors.New("accounting audit limit must be between 1 and 100")
	}
	rows, err := pool.Query(ctx, `SELECT lease_id::text FROM accounting_audit WHERE audit_seq IS NULL
		AND next_attempt_at<=now() ORDER BY next_attempt_at,created_at,lease_id LIMIT $1`, limit)
	if err != nil {
		return result, err
	}
	var leases []string
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return result, err
		}
		result.Selected++
		if !seen[id] {
			seen[id] = true
			leases = append(leases, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, err
	}
	var first error
	for _, id := range leases {
		n, err := drainLease(ctx, pool, id, limit-result.Appended)
		if err != nil {
			result.Failed++
			if first == nil {
				first = err
			}
			if ctx.Err() != nil {
				break
			}
			if retryErr := retryLease(ctx, pool, id, ErrorClass(err)); retryErr != nil {
				if first == nil {
					first = retryErr
				}
				break
			}
			continue
		}
		result.Appended += n
		if result.Appended == limit {
			break
		}
	}
	return result, first
}

func retryLease(ctx context.Context, pool *pgxpool.Pool, id, class string) error {
	_, err := pool.Exec(ctx, `UPDATE accounting_audit SET attempts=least(attempts::bigint+1,2147483647)::int,
		next_attempt_at=clock_timestamp()+make_interval(secs=>least(300,30*power(2,least(attempts,4)))::double precision),
		last_error_class=$2 WHERE lease_id=$1 AND audit_seq IS NULL`, id, class)
	return err
}

func drainLease(ctx context.Context, pool *pgxpool.Pool, id string, limit int) (int, error) {
	count := 0
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		count = 0
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('starlight-accounting:' || $1,0))`, id).
			Scan(&locked); err != nil || !locked {
			return err
		}
		var receipt string
		var hash []byte
		err := tx.QueryRow(ctx, `SELECT receipt,receipt_hash FROM terminal_leases WHERE lease_id=$1`, id).Scan(&receipt, &hash)
		if db.NotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := checkHash(receipt, hash); err != nil {
			return err
		}
		var seq *int64
		var chain []byte
		if err := tx.QueryRow(ctx, `SELECT audit_seq,audit_hash FROM accounting_audit WHERE lease_id=$1 AND kind='terminal' FOR UPDATE`, id).
			Scan(&seq, &chain); err != nil {
			return err
		}
		if seq == nil {
			e := event(id, "terminal", map[string]any{"receipt": receipt, "receipt_sha256": hex.EncodeToString(hash)})
			var err error
			seq, chain, err = appendLinked(ctx, tx, id, "terminal", e)
			if err != nil {
				return err
			}
			count++
		}
		if count == limit {
			return nil
		}
		var report string
		var reportHash []byte
		err = tx.QueryRow(ctx, `SELECT l.report,l.report_hash FROM late_usage l
			JOIN accounting_audit a USING (lease_id) WHERE l.lease_id=$1 AND a.kind='late' AND a.audit_seq IS NULL
			FOR UPDATE OF a`, id).Scan(&report, &reportHash)
		if db.NotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := checkHash(report, reportHash); err != nil {
			return err
		}
		e := event(id, "late", map[string]any{"receipt": receipt, "receipt_sha256": hex.EncodeToString(hash),
			"report": report, "report_sha256": hex.EncodeToString(reportHash),
			"terminal_audit_seq": strconv.FormatInt(*seq, 10), "terminal_audit_hash": hex.EncodeToString(chain)})
		if _, _, err := appendLinked(ctx, tx, id, "late", e); err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

func checkHash(body string, hash []byte) error {
	want := sha256.Sum256([]byte(body))
	if !bytes.Equal(want[:], hash) {
		return errors.New("accounting evidence hash mismatch")
	}
	return nil
}

func event(id, kind string, details map[string]any) audit.Event {
	e := audit.Actor(nil)
	e.Action, e.TargetType, e.TargetID, e.Details = "accounting."+kind, "lease", id, details
	return e
}

func appendLinked(ctx context.Context, tx pgx.Tx, id, kind string, e audit.Event) (*int64, []byte, error) {
	if err := audit.Append(ctx, tx, e); err != nil {
		return nil, nil, err
	}
	var seq int64
	var hash []byte
	if err := tx.QueryRow(ctx, `SELECT seq,hash FROM audit_events ORDER BY seq DESC LIMIT 1`).Scan(&seq, &hash); err != nil {
		return nil, nil, err
	}
	result, err := tx.Exec(ctx, `UPDATE accounting_audit SET audit_seq=$3,audit_hash=$4
		WHERE lease_id=$1 AND kind=$2 AND audit_seq IS NULL`, id, kind, seq, hash)
	if err != nil {
		return nil, nil, err
	}
	if result.RowsAffected() != 1 {
		return nil, nil, fmt.Errorf("accounting %s audit link was already handled", kind)
	}
	return &seq, hash, nil
}

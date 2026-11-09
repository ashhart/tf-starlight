package fleet

import (
	"context"
	"encoding/csv"
	"io"
	"strconv"
	"time"

	"github.com/ashhart/tf-starlight/internal/db"
)

// AccountingRow separates an original budget charge from a later observation of the same lease.
type AccountingRow struct {
	LeaseID, ProjectID, ModelID, Kind string
	EndedAt                           time.Time
	ReceivedAt                        *time.Time
	ChargedTokens, ChargedMicros      *int64
	OriginallyObserved                bool
	Prompt, Completion, Cached        *int64
	CostMicros                        *int64
	TerminalAudit, LateAudit          *int64
}

// AccountingUsage returns at most 1,000 receipts by terminal time, with the caller's existing project scope.
func AccountingUsage(ctx context.Context, q db.Querier, f UsageFilter) ([]AccountingRow, bool, error) {
	if !f.To.After(f.From) || f.To.Sub(f.From) > 400*24*time.Hour {
		return nil, false, Invalid("choose a range of at most 400 days that ends after it starts")
	}
	projects := f.Projects
	if projects == nil {
		projects = []string{}
	}
	rows, err := q.Query(ctx, `SELECT t.lease_id::text,coalesce(t.project_id::text,''),t.receipt::jsonb->>'model',
		t.kind,t.ended_at,l.created_at,t.charged_tokens,t.charged_micros,t.observed,
		(l.report::jsonb->>'prompt')::bigint,(l.report::jsonb->>'completion')::bigint,
		(l.report::jsonb->>'cached')::bigint,(l.report::jsonb->>'cost')::bigint,
		a.audit_seq,b.audit_seq
		FROM terminal_leases t LEFT JOIN late_usage l USING (lease_id)
		LEFT JOIN accounting_audit a ON a.lease_id=t.lease_id AND a.kind='terminal'
		LEFT JOIN accounting_audit b ON b.lease_id=t.lease_id AND b.kind='late'
		WHERE t.ended_at >= $1 AND t.ended_at < $2 AND ($3 OR t.project_id=ANY($4::uuid[]))
		ORDER BY t.ended_at DESC,t.lease_id LIMIT 1001`, f.From, f.To, f.Projects == nil, projects)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []AccountingRow
	for rows.Next() {
		var r AccountingRow
		if err := rows.Scan(&r.LeaseID, &r.ProjectID, &r.ModelID, &r.Kind, &r.EndedAt, &r.ReceivedAt,
			&r.ChargedTokens, &r.ChargedMicros, &r.OriginallyObserved, &r.Prompt, &r.Completion, &r.Cached,
			&r.CostMicros, &r.TerminalAudit, &r.LateAudit); err != nil {
			return nil, false, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > 1000 {
		return out[:1000], true, nil
	}
	return out, false, nil
}

// WriteAccountingCSV keeps unknown charges blank and labels late observations and their zero additional budget effect.
func WriteAccountingCSV(w io.Writer, rows []AccountingRow) error {
	c := csv.NewWriter(w)
	if err := c.Write([]string{"lease_id", "project_id", "model_id", "terminal_outcome", "ended_at", "charged_tokens",
		"charged_cost_micros", "originally_observed", "late", "received_at", "late_observed_prompt", "late_observed_completion",
		"late_observed_cached", "late_observed_cost_micros", "additional_budget_tokens", "additional_budget_micros",
		"terminal_audit_seq", "late_audit_seq", "audit_state"}); err != nil {
		return err
	}
	for _, r := range rows {
		received, state := "", "not_requested"
		late := r.ReceivedAt != nil
		if late {
			received = r.ReceivedAt.UTC().Format(time.RFC3339Nano)
		}
		if late || r.Kind == "reaped" || r.Kind == "legacy_deleted" {
			state = "linked"
			if r.TerminalAudit == nil || late && r.LateAudit == nil {
				state = "pending"
			}
		}
		if err := c.Write([]string{safeCell(r.LeaseID), safeCell(r.ProjectID), safeCell(r.ModelID), safeCell(r.Kind),
			r.EndedAt.UTC().Format(time.RFC3339Nano), optionalInt(r.ChargedTokens), optionalInt(r.ChargedMicros),
			strconv.FormatBool(r.OriginallyObserved), strconv.FormatBool(late), received,
			optionalInt(r.Prompt), optionalInt(r.Completion), optionalInt(r.Cached), optionalInt(r.CostMicros), "0", "0",
			optionalInt(r.TerminalAudit), optionalInt(r.LateAudit), state}); err != nil {
			return err
		}
	}
	c.Flush()
	return c.Error()
}

func optionalInt(v *int64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(*v, 10)
}

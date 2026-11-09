package fleetpages

import (
	"bytes"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ashhart/tf-starlight/internal/admin/fleet"
	"github.com/ashhart/tf-starlight/internal/console"
)

func (p *pages) accountingCSV(q *console.Request) error {
	f, page, err := usageQuery(q)
	if err != nil {
		return err
	}
	if !page.Visible {
		return forbidden()
	}
	var cursor *fleet.AccountingCursor
	query := q.R.URL.Query()
	if query.Get("before") != "" || query.Get("before_id") != "" {
		at, err := time.Parse(time.RFC3339Nano, query.Get("before"))
		if err != nil || query.Get("before_id") == "" {
			return console.Invalid("provide both a valid before timestamp and before_id cursor")
		}
		cursor = &fleet.AccountingCursor{EndedAt: at, LeaseID: query.Get("before_id")}
	}
	rows, truncated, err := fleet.AccountingPage(q.Ctx(), p.c.Pool, f, cursor)
	if err != nil {
		return friendly(err)
	}
	var buf bytes.Buffer
	if err := fleet.WriteAccountingCSV(&buf, rows); err != nil {
		return err
	}
	h := q.W.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="starlight-accounting-`+page.From+"-"+page.To+`.csv"`)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Starlight-Truncated", strconv.FormatBool(truncated))
	if truncated && len(rows) > 0 {
		last := rows[len(rows)-1]
		next := url.Values{"from": {page.From}, "to": {page.To}, "before": {last.EndedAt.UTC().Format(time.RFC3339Nano)},
			"before_id": {last.LeaseID}}
		if page.Project != "" {
			next.Set("project", page.Project)
		}
		h.Set("Link", "</console/accounting.csv?"+next.Encode()+">; rel=\"next\"")
	}
	q.W.WriteHeader(http.StatusOK)
	_, err = q.W.Write(buf.Bytes())
	return err
}

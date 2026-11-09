package fleetpages

import (
	"bytes"
	"net/http"
	"strconv"

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
	rows, truncated, err := fleet.AccountingUsage(q.Ctx(), p.c.Pool, f)
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
	q.W.WriteHeader(http.StatusOK)
	_, err = q.W.Write(buf.Bytes())
	return err
}

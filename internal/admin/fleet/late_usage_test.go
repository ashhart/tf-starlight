package fleet_test

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/admin/fleet"
	"github.com/ashhart/tf-starlight/internal/ids"
)

func TestAccountingReportBoundsAndUnknownChargeCSV(t *testing.T) {
	r := newRig(t)
	project := ids.New()
	r.exec(`INSERT INTO leases(id,request_id,gateway_id,model_id,scopes,priority,project_id,deadline)
		SELECT gen_random_uuid(),'legacy',$1,$2,'{}','standard',$3,now() FROM generate_series(1,1001)`,
		ids.New(), ids.New(), project)
	r.exec("DELETE FROM leases")
	f := fleet.UsageFilter{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour)}
	rows, truncated, err := fleet.AccountingUsage(r.ctx, r.pool, f)
	if err != nil || !truncated || len(rows) != 1000 {
		t.Fatalf("bounded report rows=%d truncated=%v: %v", len(rows), truncated, err)
	}
	last := rows[len(rows)-1]
	next, more, err := fleet.AccountingPage(r.ctx, r.pool, f, &fleet.AccountingCursor{EndedAt: last.EndedAt, LeaseID: last.LeaseID})
	if err != nil || more || len(next) != 1 || next[0].LeaseID == last.LeaseID {
		t.Fatalf("keyset cursor lost or repeated receipts: rows=%d more=%v: %v", len(next), more, err)
	}
	var buf bytes.Buffer
	if err := fleet.WriteAccountingCSV(&buf, rows[:1]); err != nil {
		t.Fatal(err)
	}
	cells, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil || len(cells) != 2 || cells[1][3] != "legacy_deleted" || cells[1][5] != "" ||
		cells[1][6] != "" || cells[1][7] != "unknown" || cells[1][8] != "false" || cells[1][18] != "pending" {
		t.Fatalf("unknown charge was presented as zero or observed: %q, %v", buf.String(), err)
	}
	for _, projects := range [][]string{{}, {ids.New()}} {
		f.Projects = projects
		if rows, _, err := fleet.AccountingUsage(r.ctx, r.pool, f); err != nil || len(rows) != 0 {
			t.Fatalf("project scope leaked %d rows: %v", len(rows), err)
		}
	}
	if _, _, err := fleet.AccountingPage(r.ctx, r.pool, f, &fleet.AccountingCursor{EndedAt: last.EndedAt, LeaseID: "invalid"}); err == nil {
		t.Fatal("invalid cursor accepted")
	}
	f.To = f.From
	if _, _, err := fleet.AccountingUsage(r.ctx, r.pool, f); err == nil {
		t.Fatal("empty range succeeded")
	}
}

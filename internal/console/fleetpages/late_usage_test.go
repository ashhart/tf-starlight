package fleetpages_test

import (
	"encoding/csv"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/levels"
)

func TestAccountingExportKeepsChargesSeparateAndProjectScoped(t *testing.T) {
	r := newRig(t)
	owner := r.user("owner@example.com", "owner", "")
	manager := r.user("manager@example.com", "", "project_admin")
	member := r.user("member@example.com", "", "member")
	otherProject := ids.New()
	r.exec("INSERT INTO projects(id,slug,name) VALUES($1,'other','Other')", otherProject)
	leases := map[string]string{}
	for _, project := range []string{r.project, otherProject} {
		a := &admission.Admitter{Pool: r.pool, GatewayID: ids.New()}
		req := admission.Request{RequestID: ids.New(), ModelID: r.model, Local: true, Class: levels.Internal,
			Priority: levels.Standard, Tokens: 100, QueueUntil: time.Now().Add(5 * time.Second),
			Who: admission.Who{PrincipalType: "user", PrincipalID: owner.user.ID, ProjectID: project}}
		l, err := a.Admit(r.ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		leases[project] = l.ID
		r.exec("UPDATE leases SET heartbeat_at=now()-interval '2 minutes' WHERE id=$1", l.ID)
		if n, err := admission.Reap(r.ctx, r.pool); err != nil || n != 1 {
			t.Fatalf("reap fixture=%d: %v", n, err)
		}
		if err := a.Settle(r.ctx, l, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, admission.Record{
			RequestID: req.RequestID, PrincipalType: "user", PrincipalID: owner.user.ID, ProjectID: project,
			Protocol: "openai.chat", Outcome: "ok", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	response, body := owner.do("GET", "/console/accounting.csv", nil)
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Starlight-Truncated") != "false" ||
		response.Header.Get("Cache-Control") != "no-store" || !strings.Contains(response.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("accounting export=%d %v", response.StatusCode, response.Header)
	}
	rows, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil || len(rows) != 3 {
		t.Fatalf("CSV rows=%d: %v", len(rows), err)
	}
	for _, row := range rows[1:] {
		if row[3] != "reaped" || row[5] != "100" || row[8] != "true" || row[10] != "3" || row[11] != "4" ||
			row[14] != "0" || row[15] != "0" || row[18] != "pending" {
			t.Fatalf("late work confused with original charge: %v", row)
		}
	}
	if code, body := manager.get("/console/accounting.csv"); code != http.StatusOK ||
		!strings.Contains(body, leases[r.project]) || strings.Contains(body, leases[otherProject]) {
		t.Fatalf("project-scoped CSV=%d: %s", code, body)
	}
	if code, _ := manager.get("/console/accounting.csv?project=" + otherProject); code != http.StatusForbidden {
		t.Fatalf("another project's export=%d", code)
	}
	if code, _ := member.get("/console/accounting.csv"); code != http.StatusForbidden {
		t.Fatalf("member export=%d", code)
	}
	if code, _ := owner.get("/console/accounting.csv?from=invalid"); code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid export range=%d", code)
	}
	if r.scalar("SELECT count(*)::text FROM usage_events") != "0" {
		t.Fatal("late observations entered the charged usage report")
	}
}

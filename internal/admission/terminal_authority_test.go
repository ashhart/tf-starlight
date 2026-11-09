package admission_test

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/levels"
)

func TestLateObservationRequiresEveryOriginalAuthorityField(t *testing.T) {
	f := newFleet(t, 1)
	a := f.admitter()
	req := f.request(levels.Standard, 5*time.Second)
	req.Who = admission.Who{PrincipalType: "user", PrincipalID: ids.New(), ProjectID: ids.New()}
	l, err := a.Admit(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	reapLease(t, f, l)
	r := map[string]any{"lease": l.ID, "model": req.ModelID, "route_id": l.Target.ReplicaID, "observed": true,
		"lease_request": req.RequestID, "lease_gateway": a.GatewayID, "lease_principal_type": req.Who.PrincipalType,
		"lease_principal_id": req.Who.PrincipalID, "lease_project_id": req.Who.ProjectID, "prompt": 3, "completion": 4,
		"cached": 0, "cost": 0, "protocol": "openai.chat", "status": 200, "outcome": "ok",
		"queue_ms": 0, "ttft_ms": 0, "duration_ms": 0}
	for _, field := range []string{"lease_request", "lease_gateway", "model", "lease_principal_type",
		"lease_principal_id", "lease_project_id", "route_id"} {
		t.Run(field, func(t *testing.T) {
			wrong := maps.Clone(r)
			wrong[field] = ids.New()
			var state string
			if err := f.pool.QueryRow(context.Background(), "SELECT starlight_settle_result($1)", wrong).Scan(&state); err == nil {
				t.Fatalf("mismatched %s accepted as %s", field, state)
			}
		})
	}
	for _, field := range []string{"prompt", "completion", "cached", "cost", "queue_ms", "ttft_ms", "duration_ms"} {
		wrong := maps.Clone(r)
		wrong[field] = nil
		var state string
		if err := f.pool.QueryRow(context.Background(), "SELECT starlight_settle_result($1)", wrong).Scan(&state); err == nil {
			t.Fatalf("null %s accepted as %s", field, state)
		}
	}
	var count int
	if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM late_usage").Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid observations created %d records: %v", count, err)
	}
	var state string
	if err := f.pool.QueryRow(context.Background(), "SELECT starlight_settle_result($1)", r).Scan(&state); err != nil || state != "late" {
		t.Fatalf("original authority rejected as %s: %v", state, err)
	}
}

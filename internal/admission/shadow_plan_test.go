package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ashhart/tf-starlight/internal/db"
	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/levels"
	"github.com/ashhart/tf-starlight/internal/routing"
	"github.com/ashhart/tf-starlight/internal/testutil"
)

type shadowQueryCapture struct {
	db.Querier
	sql  string
	args []any
}

func (q *shadowQueryCapture) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	q.sql, q.args = sql, append([]any(nil), args...)
	return q.Querier.Query(ctx, sql, args...)
}

func TestUnavailableShadowSnapshotAvoidsWideWindow(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	model, deployment, node := ids.New(), ids.New(), ids.New()
	exec("INSERT INTO models(id,name,display_name) VALUES($1,'query-probe','Query probe')", model)
	exec(`INSERT INTO deployments(id,model_id,name,spec) VALUES($1,$2,'query-probe','{"model":{"kind":"path","path":"/m"}}')`, deployment, model)
	exec(`INSERT INTO nodes(id,name,status,address,last_seen_at) VALUES($1,'query-probe','active','192.0.2.10:7443',now())`, node)
	count := routing.MaxShadowCandidates + 1
	replicas := make([]string, count)
	for i := range replicas {
		replicas[i] = ids.New()
	}
	exec(`INSERT INTO replicas(id,deployment_id,deployment_revision,node_id,state,slots,epoch)
		SELECT id,$2,1,$3,'ready',2,1 FROM unnest($1::uuid[]) AS x(id)`, replicas, deployment, node)
	payload, err := json.Marshal(map[string]any{"generation": 1, "observed_at": time.Now().UTC(),
		"window_seconds": 1, "prompt_tokens_per_second": 100, "tokens_per_second": 100,
		"running": 0, "waiting": 0, "padding": strings.Repeat("x", 3500)})
	if err != nil {
		t.Fatal(err)
	}
	exec("UPDATE replicas SET live=$1::jsonb,live_at=now()", payload)
	q := &shadowQueryCapture{Querier: pool}
	local, cloud, all, err := candidates(ctx, q, Request{ModelID: model, Class: levels.Internal, Local: true,
		ShadowWork: &routing.Work{Prompt: 100, Completion: 10}})
	if err != nil || len(local) != count || len(cloud) != 0 || len(all) != count {
		t.Fatalf("query probe changed actual candidates: local=%d cloud=%d all=%d: %v", len(local), len(cloud), len(all), err)
	}
	var raw []byte
	err = pool.QueryRow(ctx, "EXPLAIN (ANALYZE, VERBOSE, BUFFERS, TIMING OFF, FORMAT JSON) "+q.sql, q.args...).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Plan          map[string]any `json:"Plan"`
		ExecutionTime float64        `json:"Execution Time"`
	}
	if json.Unmarshal(raw, &plans) != nil || len(plans) != 1 {
		t.Fatal("query plan is not one bounded JSON plan")
	}
	var nodes []map[string]any
	var wideWindow bool
	var inspect func(map[string]any)
	inspect = func(plan map[string]any) {
		nodes = append(nodes, map[string]any{"type": plan["Node Type"], "width": plan["Plan Width"],
			"rows": plan["Actual Rows"], "loops": plan["Actual Loops"], "shared_hits": plan["Shared Hit Blocks"],
			"shared_reads": plan["Shared Read Blocks"], "subplan": plan["Subplan Name"]})
		if plan["Node Type"] == "WindowAgg" {
			for _, field := range plan["Output"].([]any) {
				wideWindow = wideWindow || strings.Contains(field.(string), "r.live")
			}
		}
		if children, ok := plan["Plans"].([]any); ok {
			for _, child := range children {
				inspect(child.(map[string]any))
			}
		}
	}
	inspect(plans[0].Plan)
	report, err := json.Marshal(map[string]any{"candidates": count, "wide_window": wideWindow,
		"execution_ms": plans[0].ExecutionTime, "nodes": nodes})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("shadow_query_plan " + string(report))
	if wideWindow {
		t.Fatal("unavailable shadow comparison carries raw observation JSON through its window-cap stage")
	}
}

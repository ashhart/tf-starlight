package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ashhart/tf-starlight/internal/db"
	"github.com/ashhart/tf-starlight/internal/routing"
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
	count := routing.MaxShadowCandidates + 1
	pool, req := shadowMetadataFixture(t, count)
	ctx := context.Background()
	q := &shadowQueryCapture{Querier: pool}
	local, cloud, all, err := candidates(ctx, q, req)
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

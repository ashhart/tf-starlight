package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/levels"
	"github.com/ashhart/tf-starlight/internal/observations"
	"github.com/ashhart/tf-starlight/internal/routing"
	"github.com/ashhart/tf-starlight/internal/testutil"
)

func TestShadowMetadataTransfersOnlyBoundedKnownFields(t *testing.T) {
	ctx := context.Background()
	for _, count := range []int{1, routing.MaxShadowCandidates, routing.MaxShadowCandidates + 1} {
		pool, req := shadowMetadataFixture(t, count)
		local, cloud, all, err := candidates(ctx, pool, req)
		if err != nil || len(local) != count || len(cloud) != 0 || len(all) != count {
			t.Fatalf("metadata cap changed actual candidates: local=%d cloud=%d all=%d: %v", len(local), len(cloud), len(all), err)
		}
		for _, candidate := range local {
			snapshot := candidate.work
			if snapshot == nil || len(snapshot.raw) > 512 || strings.Contains(string(snapshot.raw), "metadata-private-canary") ||
				strings.Contains(string(snapshot.raw), "unexpected_private_field") {
				t.Fatal("shadow snapshot transferred unknown or excessive payload fields")
			}
			if count > routing.MaxShadowCandidates {
				if string(snapshot.raw) != "{}" || snapshot.reserved != "" {
					t.Fatal("unavailable comparison fetched metadata or reservation totals beyond its cap")
				}
			} else {
				var load observations.Load
				if json.Unmarshal(snapshot.raw, &load) != nil || load.Generation != 1 || load.TokensPerSecond == nil || snapshot.reserved != "0" {
					t.Fatal("bounded metadata lost a required phase field or known zero reservation")
				}
			}
		}
	}
}

func shadowMetadataFixture(t *testing.T, count int) (*pgxpool.Pool, Request) {
	t.Helper()
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
	replicas := make([]string, count)
	for i := range replicas {
		replicas[i] = ids.New()
	}
	exec(`INSERT INTO replicas(id,deployment_id,deployment_revision,node_id,state,slots,epoch)
		SELECT id,$2,1,$3,'ready',2,1 FROM unnest($1::uuid[]) AS x(id)`, replicas, deployment, node)
	payload, err := json.Marshal(map[string]any{"generation": 1, "observed_at": time.Now().UTC(),
		"window_seconds": 1, "prompt_tokens_per_second": 100, "tokens_per_second": 100,
		"running": 0, "waiting": 0, "unexpected_private_field": "metadata-private-canary" + strings.Repeat("x", 3500)})
	if err != nil {
		t.Fatal(err)
	}
	exec("UPDATE replicas SET live=$1::jsonb,live_at=now()", payload)
	return pool, Request{ModelID: model, Class: levels.Internal, Local: true, ShadowWork: &routing.Work{Prompt: 100, Completion: 10}}
}

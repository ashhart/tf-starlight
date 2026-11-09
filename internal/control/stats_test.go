package control

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/control/wire"
	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/observations"
	"github.com/ashhart/tf-starlight/internal/testutil"
)

func TestStatsDoNotRenewOnReplayOrBorrowAnotherNodesReplica(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	model, dep, node, replica := ids.New(), ids.New(), ids.New(), ids.New()
	exec("INSERT INTO models (id,name,display_name) VALUES ($1,'model','Model')", model)
	exec("INSERT INTO deployments (id,model_id,name,spec) VALUES ($1,$2,'deployment','{}')", dep, model)
	exec("INSERT INTO nodes (id,name,status,last_seen_at) VALUES ($1,'node','active',now())", node)
	exec("INSERT INTO replicas (id,deployment_id,deployment_revision,node_id,state,slots,epoch) VALUES ($1,$2,1,$3,'ready',2,1)", replica, dep, node)
	s := &Server{Pool: pool}
	now := time.Now().UTC().Truncate(time.Microsecond)
	zero, invalid := 0.0, -1.0
	load := observations.Load{Generation: 1, ObservedAt: now.Add(-time.Second), WindowSeconds: 5, TokensPerSecond: &zero, KVUsage: &invalid}
	report := wire.ReplicaStats{ReplicaID: replica, Observation: &load}
	if err := s.recordStats(ctx, node, report, now); err != nil {
		t.Fatal(err)
	}
	read := func() (time.Time, map[string]any) {
		t.Helper()
		var at time.Time
		var raw []byte
		if err := pool.QueryRow(ctx, "SELECT live_at,live FROM replicas WHERE id=$1", replica).Scan(&at, &raw); err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if json.Unmarshal(raw, &value) != nil {
			t.Fatal("invalid stored observation")
		}
		return at, value
	}
	at, stored := read()
	if stored["tokens_per_second"] != float64(0) || stored["kv_usage"] != nil {
		t.Fatalf("zero and invalid readings merged: %v", stored)
	}
	for _, item := range []struct {
		node string
		at   time.Time
	}{{node, load.ObservedAt}, {node, load.ObservedAt.Add(-time.Second)}, {ids.New(), now}} {
		load.ObservedAt = item.at
		if err := (&Server{Pool: pool}).recordStats(ctx, item.node, report, now.Add(5*time.Second)); err != nil {
			t.Fatal(err)
		}
		if current, _ := read(); !current.Equal(at) {
			t.Fatal("replay, reordered report or foreign node refreshed the sample")
		}
	}
	load.Generation = 2
	load.ObservedAt = now
	if err := s.recordStats(ctx, node, report, now); err != nil {
		t.Fatal(err)
	}
	if current, _ := read(); !current.Equal(at) {
		t.Fatal("another generation refreshed the sample")
	}
	load.Generation = 1
	load.ObservedAt = now.Add(-20 * time.Second)
	if err := s.recordStats(ctx, node, report, now); err != nil {
		t.Fatal(err)
	}
	if current, _ := read(); !current.Equal(at) {
		t.Fatal("expired report refreshed the sample")
	}
	exec("UPDATE replicas SET desired='stopped' WHERE id=$1", replica)
	load.ObservedAt = now
	if err := s.recordStats(ctx, node, report, now); err != nil {
		t.Fatal(err)
	}
	if current, _ := read(); !current.Equal(at) {
		t.Fatal("stopped replica accepted new stats")
	}
}

func TestLegacyStatsCannotAssertMeasuredZero(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	model, dep, node, replica := ids.New(), ids.New(), ids.New(), ids.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO models (id,name,display_name) VALUES ($1,'model','Model')", []any{model}},
		{"INSERT INTO deployments (id,model_id,name,spec) VALUES ($1,$2,'deployment','{}')", []any{dep, model}},
		{"INSERT INTO nodes (id,name) VALUES ($1,'node')", []any{node}},
		{"INSERT INTO replicas (id,deployment_id,deployment_revision,node_id,state,slots,epoch,live,live_at) VALUES ($1,$2,1,$3,'ready',2,1,'{\"tokens_per_second\":0}',now())", []any{replica, dep, node}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := (&Server{Pool: pool}).recordStats(ctx, node, wire.ReplicaStats{ReplicaID: replica}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var missing bool
	if err := pool.QueryRow(ctx, "SELECT live_at IS NULL AND NOT(live ? 'tokens_per_second') FROM replicas WHERE id=$1", replica).Scan(&missing); err != nil || !missing {
		t.Fatalf("legacy zero asserted as a measurement: %v, %v", missing, err)
	}
}

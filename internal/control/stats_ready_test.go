package control

import (
	"context"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/control/wire"
	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/testutil"
)

func TestReadyAtResetsOnObservedReadyTransitionsAndNotRepeatedReadySyncs(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	model, dep, node, replica := ids.New(), ids.New(), ids.New(), ids.New()
	exec("INSERT INTO models(id,name,display_name) VALUES($1,'model','Model')", model)
	exec("INSERT INTO deployments(id,model_id,name,spec) VALUES($1,$2,'deployment','{}')", dep, model)
	exec("INSERT INTO nodes(id,name,status,last_seen_at) VALUES($1,'node','active',now())", node)
	exec("INSERT INTO replicas(id,deployment_id,deployment_revision,node_id,state,slots,epoch) VALUES($1,$2,1,$3,'ready',2,1)", replica, dep, node)
	s := &Server{Pool: pool}
	observe := func(state string) {
		t.Helper()
		err := s.observe(ctx, node, wire.SyncRequest{Replicas: []wire.ReplicaStatus{{ReplicaID: replica, Generation: 1, State: state}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	read := func() *time.Time {
		t.Helper()
		var at *time.Time
		if err := pool.QueryRow(ctx, "SELECT ready_at FROM replicas WHERE id=$1", replica).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	for _, state := range []string{wire.StateStaging, wire.StateStarting, wire.StateDraining, wire.StateFailed, wire.StateStopped} {
		t.Run(state, func(t *testing.T) {
			exec("UPDATE replicas SET state='ready',ready_at=now()-interval '1 hour' WHERE id=$1", replica)
			old := read()
			observe(wire.StateReady)
			if at := read(); at == nil || !at.Equal(*old) {
				t.Fatal("repeated ready sync reset the ready period")
			}
			observe(state)
			if read() != nil {
				t.Fatal("leaving ready retained a ready period")
			}
			observe(wire.StateReady)
			fresh := read()
			if fresh == nil || !fresh.After(*old) {
				t.Fatal("entering ready did not start a new ready period")
			}
			observe(wire.StateReady)
			if at := read(); at == nil || !at.Equal(*fresh) {
				t.Fatal("repeated ready sync changed the new ready period")
			}
		})
	}
}

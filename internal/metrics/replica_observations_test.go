package metrics

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	dto "github.com/prometheus/client_model/go"

	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/observations"
	"github.com/ashhart/tf-starlight/internal/testutil"
)

type observationRig struct {
	pool             *pgxpool.Pool
	model, dep, node string
	t                testing.TB
}

func newObservationRig(t testing.TB) observationRig {
	r := observationRig{pool: testutil.DB(t), model: ids.New(), dep: ids.New(), node: ids.New(), t: t}
	r.exec("INSERT INTO models (id,name,display_name) VALUES ($1,'model','Model')", r.model)
	r.exec("INSERT INTO deployments (id,model_id,name,spec) VALUES ($1,$2,'deployment','{}')", r.dep, r.model)
	r.exec("INSERT INTO nodes (id,name,status,last_seen_at) VALUES ($1,'node','active',now())", r.node)
	return r
}

func (r observationRig) exec(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), sql, args...); err != nil {
		r.t.Fatal(err)
	}
}

func (r observationRig) insert(id string, load observations.Load) {
	r.t.Helper()
	raw, err := json.Marshal(load)
	if err != nil {
		r.t.Fatal(err)
	}
	r.exec("INSERT INTO replicas (id,deployment_id,deployment_revision,node_id,state,slots,epoch,live,live_at) VALUES ($1,$2,1,$3,'ready',2,1,$4,now())", id, r.dep, r.node, raw)
}

func gather(t testing.TB, s *Set) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := s.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*dto.MetricFamily{}
	for _, f := range families {
		out[f.GetName()] = f
	}
	return out
}

func sampleValue(f *dto.MetricFamily, replica, measurement string) (float64, bool) {
	if f == nil {
		return 0, false
	}
	for _, metric := range f.Metric {
		r, m := "", ""
		for _, l := range metric.Label {
			if l.GetName() == "replica" {
				r = l.GetValue()
			}
			if l.GetName() == "measurement" {
				m = l.GetValue()
			}
		}
		if r == replica && m == measurement {
			return metric.Gauge.GetValue(), true
		}
	}
	return 0, false
}

func TestReplicaObservationsDistinguishUnknownExpiredAndMeasuredZero(t *testing.T) {
	r := newObservationRig(t)
	s := New()
	s.RegisterReplicaObservations(r.pool)
	zero := 0.0
	current, unknown, expired := ids.New(), ids.New(), ids.New()
	r.insert(current, observations.Load{Generation: 1, ObservedAt: time.Now().Add(-time.Second), WindowSeconds: 5, TokensPerSecond: &zero})
	r.insert(unknown, observations.Load{})
	r.insert(expired, observations.Load{Generation: 1, ObservedAt: time.Now().Add(-time.Minute), WindowSeconds: 5, TokensPerSecond: &zero})
	f := gather(t, s)
	if v, ok := sampleValue(f["starlight_replica_decode_tokens_per_second"], current, ""); !ok || v != 0 {
		t.Fatal("measured idle rate missing")
	}
	for _, id := range []string{unknown, expired} {
		if _, ok := sampleValue(f["starlight_replica_decode_tokens_per_second"], id, ""); ok {
			t.Fatal("unknown or expired rate emitted as zero")
		}
		if v, ok := sampleValue(f["starlight_replica_observation_valid"], id, "decode"); !ok || v != 0 {
			t.Fatal("invalidity missing")
		}
	}
	if v, _ := sampleValue(f["starlight_replica_observation_age_seconds"], expired, ""); v < 59 {
		t.Fatal("expired sample age missing")
	}
	r.exec("UPDATE replicas SET ready_at=now() WHERE id=$1", current)
	f = gather(t, s)
	if _, ok := sampleValue(f["starlight_replica_decode_tokens_per_second"], current, ""); ok {
		t.Fatal("previous process sample survived a new ready period")
	}
	r.exec("UPDATE replicas SET ready_at=NULL,generation=2 WHERE id=$1", current)
	f = gather(t, s)
	if _, ok := sampleValue(f["starlight_replica_decode_tokens_per_second"], current, ""); ok {
		t.Fatal("previous assignment sample survived a new generation")
	}
	r.exec("UPDATE replicas SET desired='stopped' WHERE id=$1", current)
	f = gather(t, s)
	if _, ok := sampleValue(f["starlight_replica_observation_fresh"], current, ""); ok {
		t.Fatal("stopped replica retained a label set")
	}
	r.exec("UPDATE nodes SET last_seen_at=now()-interval '2 minutes' WHERE id=$1", r.node)
	if f = gather(t, s); f["starlight_replica_observation_fresh"] != nil {
		t.Fatal("silent node's replicas retained")
	}
}

func TestReplicaObservationChurnHasABoundedScrapeAndNoRetainedLabels(t *testing.T) {
	r := newObservationRig(t)
	s := New()
	s.RegisterReplicaObservations(r.pool)
	r.exec(`INSERT INTO replicas (id,deployment_id,deployment_revision,node_id,state,slots,epoch,live,live_at)
		SELECT gen_random_uuid(),$1,1,$2,'ready',2,1,jsonb_build_object('observed_at',now(),'generation',1,'window_seconds',5,'tokens_per_second',0,'prompt_tokens_per_second',0,'running',0,'waiting',0,'kv_usage',0),now()
		FROM generate_series(1,$3::int)`, r.dep, r.node, ReplicaObservationLimit+1)
	f := gather(t, s)
	count := 0
	for name, family := range f {
		if strings.HasPrefix(name, "starlight_replica_") {
			count += len(family.Metric)
		}
	}
	if count != 12*ReplicaObservationLimit+2 {
		t.Fatalf("series budget %d, want %d", count, 12*ReplicaObservationLimit+2)
	}
	if len(f["starlight_replica_observation_fresh"].Metric) != ReplicaObservationLimit {
		t.Fatal("replica cap was not enforced")
	}
	if v, _ := sampleValue(f["starlight_replica_observation_truncated"], "", ""); v != 1 {
		t.Fatal("truncation was not reported")
	}
	for round := 0; round < 3; round++ {
		r.exec("DELETE FROM replicas")
		id := ids.New()
		zero := 0
		r.insert(id, observations.Load{Generation: 1, ObservedAt: time.Now(), Running: &zero})
		f = gather(t, s)
		if len(f["starlight_replica_observation_fresh"].Metric) != 1 {
			t.Fatal("historical replica labels accumulated")
		}
		if _, ok := sampleValue(f["starlight_replica_observation_fresh"], id, ""); !ok {
			t.Fatal("new replica absent")
		}
	}
	r.pool.Close()
	f = gather(t, s)
	if v, _ := sampleValue(f["starlight_replica_observation_scrape_success"], "", ""); v != 0 {
		t.Fatal("failed query reported success")
	}
	if f["starlight_replica_observation_fresh"] != nil {
		t.Fatal("failed query replayed a cached observation")
	}
}

func BenchmarkReplicaObservationScrape(b *testing.B) {
	r := newObservationRig(b)
	s := New()
	s.RegisterReplicaObservations(r.pool)
	r.exec(`INSERT INTO replicas (id,deployment_id,deployment_revision,node_id,state,slots,epoch,live,live_at)
		SELECT gen_random_uuid(),$1,1,$2,'ready',2,1,jsonb_build_object('observed_at',now(),'generation',1,'window_seconds',5,'tokens_per_second',0,'prompt_tokens_per_second',0,'running',0,'waiting',0,'kv_usage',0),now()
		FROM generate_series(1,$3::int)`, r.dep, r.node, ReplicaObservationLimit)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		gather(b, s)
	}
}

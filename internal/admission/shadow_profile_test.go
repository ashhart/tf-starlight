package admission_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/levels"
	"github.com/ashhart/tf-starlight/internal/routing"
)

func TestRoutingShadowOverhead(t *testing.T) {
	if os.Getenv("STARLIGHT_SHADOW_OVERHEAD") != "1" {
		t.Skip("set STARLIGHT_SHADOW_OVERHEAD=1 for matched PostgreSQL shadow profiles")
	}
	for _, scenario := range []struct {
		count int
		state string
	}{{1, "fresh"}, {32, "fresh"}, {128, "fresh"}, {129, "fresh"},
		{128, "missing"}, {128, "stale"}, {128, "oversized"}} {
		t.Run(fmt.Sprintf("%d-%s", scenario.count, scenario.state), func(t *testing.T) {
			f := shadowProfileFleet(t, scenario.count)
			a := f.admitter()
			ctx := context.Background()
			chosen, total := "", 0
			for trial, enabled := range []bool{false, true, true, false} {
				shadowProfileState(t, f, scenario.state)
				var admitTimes, cycleTimes []float64
				const warmup, samples = 10, 100
				for i := range warmup + samples {
					if i > 0 && i%25 == 0 {
						shadowProfileState(t, f, scenario.state)
					}
					req := f.request(levels.Standard, 5*time.Second)
					req.Affinity = 77
					req.Budgets = []admission.Counter{{Key: "tokens:day:project:profile",
						Window: time.Now().UTC().Truncate(24 * time.Hour), Limit: 10000000, Amount: 100}}
					if enabled {
						req.ShadowWork = &routing.Work{Prompt: 100, Completion: 10}
					}
					start := time.Now()
					lease, err := a.Admit(ctx, req)
					admitted := time.Since(start).Seconds()
					if err != nil {
						t.Fatal(err)
					}
					if chosen == "" {
						chosen = lease.Target.ReplicaID
					}
					if lease.Target.ReplicaID != chosen {
						t.Fatal("shadow flag or observation state changed the actual affinity target")
					}
					if enabled {
						want := "ready"
						switch {
						case scenario.count > routing.MaxShadowCandidates:
							want = "too_many_candidates"
						case scenario.state != "fresh":
							want = "unknown_observation"
						case scenario.count == 1:
							want = "no_alternative"
						}
						if lease.Shadow == nil || lease.Shadow.Reason != want {
							t.Fatalf("profile did not exercise %s: %+v", want, lease.Shadow)
						}
					} else if lease.Shadow != nil {
						t.Fatal("disabled profile retained a comparison")
					}
					if err := a.Settle(ctx, lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record()); err != nil {
						t.Fatal(err)
					}
					total++
					if i >= warmup {
						admitTimes = append(admitTimes, admitted)
						cycleTimes = append(cycleTimes, time.Since(start).Seconds())
					}
				}
				shadowProfileReport(t, map[string]any{"basis": "real PostgreSQL sequential admission; no engine or production SLO",
					"candidates": scenario.count, "observation": scenario.state, "enabled": enabled, "trial": trial,
					"warmup": warmup, "samples": samples, "occupied_slots_per_replica": 1,
					"admit_p50_seconds": shadowProfilePercentile(admitTimes, 50),
					"admit_p95_seconds": shadowProfilePercentile(admitTimes, 95),
					"cycle_p50_seconds": shadowProfilePercentile(cycleTimes, 50),
					"cycle_p95_seconds": shadowProfilePercentile(cycleTimes, 95)})
			}
			var events, used, reserved, leases int
			err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_events),used,reserved,
				(SELECT count(*) FROM leases) FROM counters WHERE key='tokens:day:project:profile'`).Scan(&events, &used, &reserved, &leases)
			if err != nil || events != total || used != total*7 || reserved != 0 || leases != scenario.count {
				t.Fatalf("profile changed accounting: events=%d used=%d reserved=%d leases=%d: %v", events, used, reserved, leases, err)
			}
		})
	}
}

func shadowProfileFleet(t *testing.T, count int) fleet {
	t.Helper()
	f := newFleet(t, 8)
	ctx := context.Background()
	var node string
	if err := f.pool.QueryRow(ctx, "SELECT node_id FROM replicas WHERE id=$1", f.replica).Scan(&node); err != nil {
		t.Fatal(err)
	}
	replicas := []string{f.replica}
	for range count - 1 {
		id := ids.New()
		f.exec(t, `INSERT INTO replicas(id,deployment_id,deployment_revision,node_id,state,slots,epoch)
			VALUES($1,$2,1,$3,'ready',8,1)`, id, f.deployment, node)
		replicas = append(replicas, id)
	}
	a := f.admitter()
	var held []*admission.Lease
	t.Cleanup(func() {
		for _, lease := range held {
			if err := a.Settle(ctx, lease, admission.Usage{Observed: true}, record()); err != nil {
				t.Error(err)
			}
		}
		var remaining int
		if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM leases").Scan(&remaining); err != nil || remaining != 0 {
			t.Errorf("profile cleanup left leases=%d: %v", remaining, err)
		}
	})
	for _, id := range replicas {
		req := f.request(levels.Standard, 5*time.Second)
		req.Only = id
		lease, err := a.Admit(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, lease)
	}
	return f
}

func shadowProfileState(t *testing.T, f fleet, state string) {
	t.Helper()
	f.exec(t, "UPDATE nodes SET last_seen_at=now()")
	f.exec(t, "UPDATE leases SET heartbeat_at=now()")
	f.exec(t, `UPDATE replicas SET live=jsonb_build_object('generation',generation,'observed_at',now(),
		'window_seconds',1,'prompt_tokens_per_second',100,'tokens_per_second',100,
		'running',1,'waiting',0,'padding',repeat('x',3700)),live_at=now()`)
	switch state {
	case "missing":
		f.exec(t, "UPDATE replicas SET live='{}'")
	case "stale":
		f.exec(t, "UPDATE replicas SET live=jsonb_set(live,'{observed_at}',to_jsonb(now()-interval '1 minute'))")
	case "oversized":
		f.exec(t, "UPDATE replicas SET live=jsonb_set(live,'{padding}',to_jsonb(repeat('x',5000)))")
	}
}

func shadowProfilePercentile(values []float64, percentile int) float64 {
	slices.Sort(values)
	return values[(len(values)*percentile+99)/100-1]
}

func shadowProfileReport(t *testing.T, report map[string]any) {
	t.Helper()
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("shadow_admission_profile " + string(b))
}

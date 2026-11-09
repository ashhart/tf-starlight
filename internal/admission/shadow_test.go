package admission_test

import (
	"context"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/authn"
	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/levels"
	"github.com/ashhart/tf-starlight/internal/routing"
)

func shadowFleet(t *testing.T) (fleet, string, uint64) {
	t.Helper()
	f := newFleet(t, 2)
	fast, node := ids.New(), ids.New()
	f.exec(t, `INSERT INTO nodes(id,name,status,address,last_seen_at) VALUES($1,'fast','active','192.0.2.11:7443',now())`, node)
	f.exec(t, `INSERT INTO replicas(id,deployment_id,deployment_revision,node_id,state,slots,epoch)
		VALUES($1,$2,1,$3,'ready',2,1)`, fast, f.deployment, node)
	f.exec(t, `UPDATE replicas SET live=jsonb_build_object('generation',generation,'observed_at',now(),
		'window_seconds',1,'prompt_tokens_per_second',100,'tokens_per_second',CASE WHEN id=$1 THEN 1 ELSE 100 END,
		'running',0,'waiting',0),live_at=now()`, f.replica)
	candidates := []routing.Candidate{{ID: f.replica, Free: 2, Slots: 2}, {ID: fast, Free: 2, Slots: 2}}
	for key := uint64(1); key <= 1024; key++ {
		if routing.Pick(candidates, key) == 0 {
			return f, fast, key
		}
	}
	t.Fatal("could not choose a deterministic affinity fixture")
	return f, fast, 0
}

func TestShadowRoutingKeepsAffinityAndOneBudgetEffect(t *testing.T) {
	f, fast, affinity := shadowFleet(t)
	a := f.admitter()
	for _, enabled := range []bool{false, true} {
		req := f.request(levels.Standard, 5*time.Second)
		req.Affinity = affinity
		req.Budgets = []admission.Counter{{Key: "tokens:day:project:shadow", Window: time.Now().UTC().Truncate(24 * time.Hour),
			Limit: 10000, Amount: 100}}
		if enabled {
			req.ShadowWork = &routing.Work{Prompt: 100, Completion: 10}
		}
		lease, err := a.Admit(context.Background(), req)
		if err != nil || lease.Target.ReplicaID != f.replica {
			t.Fatalf("shadow=%v changed actual affinity: lease=%+v, %v", enabled, lease, err)
		}
		if enabled {
			d := lease.Shadow
			if d == nil || d.Reason != "ready" || d.ActualID != f.replica || d.ProposedID != fast || d.ActualSeconds != 11 {
				t.Fatalf("missing advisory disagreement: %+v", d)
			}
		} else if lease.Shadow != nil {
			t.Fatal("default-off admission produced a shadow decision")
		}
		if err := a.Settle(context.Background(), lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record()); err != nil {
			t.Fatal(err)
		}
	}
	var events, used, reserved, leases int64
	err := f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM usage_events),used,reserved,
		(SELECT count(*) FROM leases) FROM counters WHERE key='tokens:day:project:shadow'`).Scan(&events, &used, &reserved, &leases)
	if err != nil || events != 2 || used != 14 || reserved != 0 || leases != 0 {
		t.Fatalf("shadow changed accounting: events=%d used=%d reserved=%d leases=%d: %v", events, used, reserved, leases, err)
	}
}

func TestUnknownShadowMeasurementsNeverRefuseOrReroute(t *testing.T) {
	for _, change := range []string{
		"UPDATE replicas SET live='{}'",
		"UPDATE replicas SET generation=generation+1",
		"UPDATE replicas SET live=jsonb_set(live,'{observed_at}',to_jsonb(now()-interval '1 minute'))",
		"UPDATE replicas SET live=jsonb_set(live,'{tokens_per_second}','\"invalid\"')",
		"UPDATE replicas SET live=jsonb_set(live,'{padding}',to_jsonb(repeat('x',5000)))",
	} {
		t.Run(change, func(t *testing.T) {
			f, _, affinity := shadowFleet(t)
			f.exec(t, change)
			a := f.admitter()
			req := f.request(levels.Standard, 5*time.Second)
			req.Affinity, req.ShadowWork = affinity, &routing.Work{Prompt: 100, Completion: 10}
			lease, err := a.Admit(context.Background(), req)
			if err != nil || lease.Target.ReplicaID != f.replica || lease.Shadow == nil || lease.Shadow.Reason != "unknown_observation" {
				t.Fatalf("unknown observation changed serving: lease=%+v, %v", lease, err)
			}
			if err := a.Settle(context.Background(), lease, admission.Usage{Observed: true}, record()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestShadowUsesEligibleTargetsOnly(t *testing.T) {
	f, fast, affinity := shadowFleet(t)
	f.exec(t, "UPDATE nodes SET max_classification='public' WHERE id=(SELECT node_id FROM replicas WHERE id=$1)", fast)
	a := f.admitter()
	req := f.request(levels.Standard, 5*time.Second)
	req.Affinity, req.ShadowWork = affinity, &routing.Work{Prompt: 100, Completion: 10}
	lease, err := a.Admit(context.Background(), req)
	if err != nil || lease.Target.ReplicaID != f.replica || lease.Shadow == nil || lease.Shadow.Reason != "no_alternative" {
		t.Fatalf("shadow considered a forbidden target: lease=%+v, %v", lease, err)
	}
	if err := a.Settle(context.Background(), lease, admission.Usage{Observed: true}, record()); err != nil {
		t.Fatal(err)
	}
}

func TestShadowCapUsesTheSamePinnedAndExcludedCandidates(t *testing.T) {
	f := newFleet(t, 2)
	ctx := context.Background()
	other := ids.New()
	f.exec(t, `INSERT INTO deployments(id,model_id,name,spec)
		SELECT $1,model_id,'other',spec FROM deployments WHERE id=$2`, other, f.deployment)
	var node string
	if err := f.pool.QueryRow(ctx, "SELECT node_id FROM replicas WHERE id=$1", f.replica).Scan(&node); err != nil {
		t.Fatal(err)
	}
	extra := make([]string, routing.MaxShadowCandidates)
	for i := range extra {
		extra[i] = ids.New()
	}
	f.exec(t, `INSERT INTO replicas(id,deployment_id,deployment_revision,node_id,state,slots,epoch)
		SELECT id,$2,1,$3,'ready',2,1 FROM unnest($1::uuid[]) AS x(id)`, extra, other, node)
	f.exec(t, `UPDATE replicas SET live=jsonb_build_object('generation',generation,'observed_at',now(),
		'window_seconds',1,'prompt_tokens_per_second',100,'tokens_per_second',100,'running',0,'waiting',0),live_at=now()`)
	a := f.admitter()
	for _, mode := range []string{"only", "exclude", "pin"} {
		t.Run(mode, func(t *testing.T) {
			for _, enabled := range []bool{false, true} {
				req := f.request(levels.Standard, 5*time.Second)
				switch mode {
				case "only":
					req.Only = f.replica
				case "exclude":
					req.Exclude = extra
				case "pin":
					req.Pins = []authn.Pin{{Deployment: f.deployment, Revision: 1}}
				}
				if enabled {
					req.ShadowWork = &routing.Work{Prompt: 100, Completion: 10}
				}
				lease, err := a.Admit(ctx, req)
				if err != nil || lease.Target.ReplicaID != f.replica {
					t.Fatalf("shadow=%v changed filtered eligibility: %+v %v", enabled, lease, err)
				}
				if enabled && (lease.Shadow == nil || lease.Shadow.Reason != "no_alternative") {
					t.Fatalf("cap counted filtered-out candidates: %+v", lease.Shadow)
				}
				if !enabled && lease.Shadow != nil {
					t.Fatal("disabled admission produced metadata")
				}
				if err := a.Settle(ctx, lease, admission.Usage{Observed: true}, record()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	var leases, events int
	if err := f.pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM leases),(SELECT count(*) FROM usage_events)").
		Scan(&leases, &events); err != nil || leases != 0 || events != 6 {
		t.Fatalf("filtered profiles changed accounting: leases=%d events=%d: %v", leases, events, err)
	}
}

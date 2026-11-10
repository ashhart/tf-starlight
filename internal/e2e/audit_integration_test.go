package e2e_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/config"
	"github.com/ashhart/tf-starlight/internal/telemetry"
	"github.com/ashhart/tf-starlight/internal/testutil/tracecollector"
)

func TestAuditFollowupsPreserveCombinedServingAndAccounting(t *testing.T) {
	var expected string
	for _, mode := range []struct {
		shadow bool
		traces bool
	}{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("shadow-%v-traces-%v", mode.shadow, mode.traces), func(t *testing.T) {
			collector := tracecollector.New(t)
			f := startStarlight(t, func(c *config.Config) {
				c.Gateway.RoutingShadow = mode.shadow
				c.Database.MaxConns = 32
				if mode.traces {
					c.Telemetry = config.Telemetry{Endpoint: collector.URL(), SamplePerMillion: 1000000,
						Queue: 4096, Batch: 32, Timeout: config.Duration{Duration: time.Second}}
				}
			})
			exec(t, f.pool, "UPDATE deployments SET spec=jsonb_set(spec,'{parallel}','16'),min_replicas=2,max_replicas=2")
			opts := nodeOptions{env: []string{"FAKE_ENGINE_DELAY_MS=0"}}
			if mode.traces {
				opts.telemetry = telemetry.Options{Endpoint: collector.URL(), SamplePerMillion: 1000000, Queue: 4096, Batch: 32}
			}
			for i := 1; i <= 2; i++ {
				f.enrolWith(f.nodeURL, fmt.Sprintf("combined-node-%d", i), i, opts)
			}
			exec(t, f.pool, `UPDATE replicas SET live=jsonb_build_object('generation',generation,'observed_at',now(),
				'window_seconds',1,'prompt_tokens_per_second',100,'tokens_per_second',100,'running',0,'waiting',0),live_at=now()`)
			concurrentTraceBurst(t, f, 1, 1, &expected)
			const samples, workers, total = 64, 8, 65
			_, elapsed := concurrentTraceBurst(t, f, samples, workers, &expected)
			charged := combinedAccounting(t, f, total)
			var duplicate bool
			if err := f.pool.QueryRow(f.ctx, `SELECT bool_and(starlight_settle_result(
				jsonb_build_object('lease',lease_id,'observed',true))='complete') FROM terminal_leases`).Scan(&duplicate); err != nil || !duplicate {
				t.Fatalf("terminal receipt retry changed its completed result: %v", err)
			}
			if combinedAccounting(t, f, total) != charged {
				t.Fatal("terminal retry charged or recorded usage twice")
			}
			decisions, ready, tokenSamples := combinedShadowMetrics(t, f)
			want := 0.0
			if mode.shadow {
				want = total
			}
			if decisions != want || mode.shadow && ready == 0 {
				t.Fatalf("combined shadow did not observe actual committed requests: decisions=%v ready=%v", decisions, ready)
			}
			for _, phase := range []string{"prompt", "completion"} {
				if tokenSamples[phase] != uint64(want) {
					t.Fatalf("combined token calibration phase=%s samples=%d want=%v", phase, tokenSamples[phase], want)
				}
			}
			frames, posts, quiet := concurrentTraceSnapshot(t, collector)
			traces := map[string]int{}
			for _, frame := range frames {
				traces[frame.Trace]++
			}
			if mode.traces {
				if !quiet || len(traces) != total || len(frames) != 11*total {
					t.Fatalf("combined flags lost complete traces: traces=%d spans=%d quiet=%v", len(traces), len(frames), quiet)
				}
				for _, spans := range traces {
					if spans != 11 {
						t.Fatal("combined flags exported a partial nonstreaming trace")
					}
				}
			} else if posts != 0 || len(frames) != 0 {
				t.Fatal("disabled combined exporter contacted the collector")
			}
			collector.AssertPrivate(t, "concurrent-private-prompt", "concurrent-private-identity", f.key, f.project, f.user.ID, f.model)
			concurrentTraceReport(t, "audit_combined_qualification", map[string]any{
				"basis":          "two fake-engine nodes, injected synthetic phase observations; no device or production SLO",
				"shadow_enabled": mode.shadow, "tracing_enabled": mode.traces, "workers": workers, "samples": samples,
				"warmup": 1, "wall_seconds": elapsed.Seconds(), "choices_and_usage_equal": true,
				"usage_rows": total, "terminal_receipts": total, "late_rows": 0, "leases": 0,
				"charged_tokens": charged, "duplicate_terminal_result": "complete", "shadow_decisions": decisions,
				"comparable_decisions": ready, "token_calibration_samples": tokenSamples,
				"complete_traces": len(traces), "accepted_spans": len(frames)})
		})
	}
}

func combinedAccounting(t *testing.T, f *fleet, count int) int64 {
	t.Helper()
	var events, receipts, matched, leases, late int
	var charged int64
	err := f.pool.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM usage_events),(SELECT count(*) FROM terminal_leases),
		(SELECT count(*) FROM usage_events u JOIN terminal_leases t
		ON t.receipt::jsonb->>'request_hash'=encode(sha256(convert_to(u.request_id,'UTF8')),'hex')
		WHERE t.kind='settled' AND t.observed AND t.project_id=u.project_id AND t.charged_tokens=u.prompt_tokens+u.completion_tokens
		AND t.charged_micros=u.cost_micros AND t.receipt::jsonb->>'replica'=u.route_id::text),
		(SELECT count(*) FROM leases),(SELECT count(*) FROM late_usage),
		(SELECT coalesce(sum(charged_tokens),0)::bigint FROM terminal_leases)`).
		Scan(&events, &receipts, &matched, &leases, &late, &charged)
	if err != nil || events != count || receipts != count || matched != count || leases != 0 || late != 0 || charged <= 0 {
		t.Fatalf("combined accounting mismatch: usage=%d receipts=%d matched=%d leases=%d late=%d: %v",
			events, receipts, matched, leases, late, err)
	}
	return charged
}

func combinedShadowMetrics(t *testing.T, f *fleet) (float64, float64, map[string]uint64) {
	t.Helper()
	families, err := f.starlight.Metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var decisions, ready float64
	tokenSamples := map[string]uint64{}
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), "starlight_routing_shadow_") {
			continue
		}
		for _, metric := range family.Metric {
			comparable := false
			phase := ""
			tokenMetric := family.GetName() == "starlight_routing_shadow_token_error_tokens"
			if tokenMetric && len(metric.Label) != 3 {
				t.Fatal("token calibration exported an unexpected label set")
			}
			for _, label := range metric.Label {
				if tokenMetric {
					switch label.GetName() {
					case "model":
					case "phase":
						phase = label.GetValue()
						if phase != "prompt" && phase != "completion" {
							t.Fatal("token calibration exported an unbounded phase")
						}
					case "direction":
						value := label.GetValue()
						if value != "under" && value != "over" && value != "equal" {
							t.Fatal("token calibration exported an unbounded direction")
						}
					default:
						t.Fatal("token calibration exported an identity label")
					}
				} else {
					switch label.GetName() {
					case "model", "reason", "agreement", "choice", "result":
					default:
						t.Fatal("combined shadow exported an unbounded identity label")
					}
				}
				for _, private := range []string{"concurrent-private-prompt", "concurrent-private-identity", f.key, f.project, f.user.ID, f.model} {
					if strings.Contains(label.GetValue(), private) {
						t.Fatal("combined shadow exported private request content or identity")
					}
				}
				comparable = comparable || label.GetName() == "reason" && label.GetValue() == "ready"
			}
			if tokenMetric {
				if phase == "" {
					t.Fatal("token calibration lost its phase label")
				}
				tokenSamples[phase] += metric.GetHistogram().GetSampleCount()
			}
			if family.GetName() == "starlight_routing_shadow_decisions_total" {
				decisions += metric.GetCounter().GetValue()
				if comparable {
					ready += metric.GetCounter().GetValue()
				}
			}
		}
	}
	return decisions, ready, tokenSamples
}

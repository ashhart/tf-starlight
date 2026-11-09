package e2e_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/config"
)

func TestRoutingShadowOverhead(t *testing.T) {
	if os.Getenv("STARLIGHT_SHADOW_OVERHEAD") != "1" {
		t.Skip("set STARLIGHT_SHADOW_OVERHEAD=1 for matched fake-engine gateway profiles")
	}
	var expected string
	for trial, enabled := range []bool{false, true, true, false} {
		t.Run(fmt.Sprintf("%d-%v", trial, enabled), func(t *testing.T) {
			f := startStarlight(t, func(c *config.Config) { c.Gateway.RoutingShadow = enabled })
			f.enrolWith(f.nodeURL, "profile-node", 1, nodeOptions{env: []string{"FAKE_ENGINE_DELAY_MS=0"}})
			const warmup, samples = 20, 200
			var times []float64
			for i := range warmup + samples {
				start := time.Now()
				response, body := f.call(http.MethodPost, "/v1/chat/completions", f.key,
					`{"model":"qwen3.8-27b","max_tokens":8,"messages":[{"role":"user","content":"profile fixture"}]}`)
				elapsed := time.Since(start).Seconds()
				var result struct {
					Choices json.RawMessage `json:"choices"`
					Usage   json.RawMessage `json:"usage"`
				}
				if response.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &result) != nil ||
					len(result.Choices) == 0 || len(result.Usage) == 0 || response.Header.Get("X-Starlight-Route") != "local" {
					t.Fatalf("shadow profile changed serving: %d %s", response.StatusCode, body)
				}
				canonical, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				if expected == "" {
					expected = string(canonical)
				}
				if string(canonical) != expected {
					t.Fatal("shadow flag changed choices or observed usage")
				}
				if i >= warmup {
					times = append(times, elapsed)
				}
			}
			var events, leases int
			if err := f.pool.QueryRow(f.ctx, "SELECT (SELECT count(*) FROM usage_events),(SELECT count(*) FROM leases)").
				Scan(&events, &leases); err != nil || events != warmup+samples || leases != 0 {
				t.Fatalf("shadow profile changed accounting: events=%d leases=%d: %v", events, leases, err)
			}
			decisions := shadowProfileDecisions(t, f)
			if enabled && decisions != warmup+samples || !enabled && decisions != 0 {
				t.Fatalf("profile did not exercise configured shadow path: enabled=%v decisions=%v", enabled, decisions)
			}
			slices.Sort(times)
			report := map[string]any{"basis": "single-host sequential gateway/node/fake-engine; no device or production SLO",
				"enabled": enabled, "trial": trial, "warmup": warmup, "samples": samples,
				"p50_seconds": times[(len(times)+1)/2-1], "p95_seconds": times[(len(times)*95+99)/100-1],
				"decisions": decisions, "usage_rows": events, "leases": leases, "choices_and_usage_equal": true}
			b, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			t.Log("shadow_gateway_profile " + string(b))
		})
	}
}

func shadowProfileDecisions(t *testing.T, f *fleet) float64 {
	t.Helper()
	families, err := f.starlight.Metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, family := range families {
		if family.GetName() == "starlight_routing_shadow_decisions_total" {
			for _, metric := range family.Metric {
				total += metric.GetCounter().GetValue()
			}
		}
	}
	return total
}

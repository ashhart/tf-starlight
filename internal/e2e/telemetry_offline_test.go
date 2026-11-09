package e2e_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/config"
	"github.com/ashhart/tf-starlight/internal/telemetry"
)

func TestOfflineModeWithholdsRemotePhaseExport(t *testing.T) {
	endpoint := "http://collector.invalid/v1/traces"
	f := startStarlight(t, func(c *config.Config) {
		c.Telemetry = config.Telemetry{Endpoint: endpoint, SamplePerMillion: 1000000, Queue: 32, Batch: 1}
	})
	exec(t, f.pool, "UPDATE settings SET offline=true WHERE id")
	f.starlight.Policies.Flush()
	f.enrolWith(f.nodeURL, "offline-trace-node", 1, nodeOptions{env: []string{"FAKE_ENGINE_DELAY_MS=0"},
		telemetry: telemetry.Options{Endpoint: endpoint, SamplePerMillion: 1000000, Queue: 32, Batch: 1}})
	response, _ := f.call(http.MethodPost, "/v1/chat/completions", f.key,
		`{"model":"qwen3.8-27b","max_tokens":2,"messages":[{"role":"user","content":"local fixture"}]}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("offline tracing changed local serving: %d", response.StatusCode)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		families, err := f.starlight.Metrics.Registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		var policy, requests float64
		for _, family := range families {
			for _, metric := range family.Metric {
				switch family.GetName() {
				case "starlight_phase_telemetry_policy_total":
					policy += metric.GetCounter().GetValue()
				case "starlight_phase_telemetry_requests_total":
					requests += metric.GetCounter().GetValue()
				}
			}
		}
		if requests != 0 {
			t.Fatal("offline exporter attempted a remote request")
		}
		if policy > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("offline export losses were not observable")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

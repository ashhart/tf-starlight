package e2e_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/config"
	"github.com/ashhart/tf-starlight/internal/telemetry"
	"github.com/ashhart/tf-starlight/internal/testutil/tracecollector"
)

func tracedFleet(t *testing.T, collector *tracecollector.Collector, rate int, delay string) *fleet {
	t.Helper()
	f := startStarlight(t, func(c *config.Config) {
		c.Telemetry = config.Telemetry{Endpoint: collector.URL(), SamplePerMillion: rate, Queue: 4096,
			Batch: 32, Timeout: config.Duration{Duration: time.Second}}
		c.Metrics.StreamTimings = true
	})
	f.enrolWith(f.nodeURL, "trace-node", 1, nodeOptions{env: []string{"FAKE_ENGINE_DELAY_MS=" + delay},
		telemetry: telemetry.Options{Endpoint: collector.URL(), SamplePerMillion: rate, Queue: 4096, Batch: 32}})
	return f
}

func TestGatewayAndNodePhasesAreConnectedAndContentFree(t *testing.T) {
	collector := tracecollector.New(t)
	f := tracedFleet(t, collector, 1000000, "25")
	response, body := f.call(http.MethodPost, "/v1/chat/completions", f.key,
		`{"model":"qwen3.8-27b","max_tokens":2,"stream":true,"messages":[{"role":"user","content":"private-prompt-canary"}],`+
			`"tools":[{"type":"function","function":{"name":"private-tool-canary","parameters":{"type":"object"}}}]}`,
		"X-Request-Id", "private-client-id", "Traceparent", "00-11111111111111111111111111111111-1111111111111111-01",
		"Tracestate", "vendor=private-vendor-canary", "Baggage", "private-identity-canary")
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "[DONE]") {
		t.Fatalf("phase tracing changed serving: %d %s", response.StatusCode, body)
	}
	request := response.Header.Get("X-Request-Id")
	if !strings.HasPrefix(request, "req_") || request == "private-client-id" {
		t.Fatal("public context bypassed fresh request-ID generation")
	}
	id := strings.TrimPrefix(request, "req_")
	frames := collector.Wait(t, id, "gateway.request", "gateway.prepare", "gateway.admission", "gateway.dispatch",
		"gateway.relay", "gateway.settlement", "gateway.first_data", "gateway.first_output", "gateway.first_flush",
		"node.request", "node.permit", "node.engine_request", "node.engine_headers", "node.engine_relay")
	root, dispatch, node := frames["gateway.request"], frames["gateway.dispatch"], frames["node.request"]
	if root.Parent != "" || dispatch.Parent != root.ID || node.Parent != dispatch.ID ||
		frames["node.engine_request"].Parent != node.ID {
		t.Fatal("gateway, node and engine HTTP phases do not form one parent-linked trace")
	}
	if frames["gateway.first_data"].End > frames["gateway.first_output"].End ||
		frames["gateway.first_output"].End > frames["gateway.first_flush"].End {
		t.Fatal("gateway cumulative boundaries lost their causal order")
	}
	if d := time.Duration(frames["gateway.relay"].End - frames["gateway.relay"].Start); d < 50*time.Millisecond {
		t.Fatalf("controlled stream delays did not enter relay time: %v", d)
	}
	collector.AssertPrivate(t, "private-prompt-canary", "private-tool-canary", "private-client-id", "private-vendor-canary",
		"private-identity-canary", "11111111111111111111111111111111", f.key, f.project, f.user.ID, f.model)
	var events, leases int
	if err := f.pool.QueryRow(f.ctx, "SELECT (SELECT count(*) FROM usage_events),(SELECT count(*) FROM leases)").
		Scan(&events, &leases); err != nil || events != 1 || leases != 0 {
		t.Fatalf("telemetry changed settlement: events=%d leases=%d: %v", events, leases, err)
	}
}

func TestDefaultOffAndZeroSamplePreserveServingWithoutExport(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		name := "zero-sample"
		if disabled {
			name = "disabled"
		}
		t.Run(name, func(t *testing.T) {
			collector := tracecollector.New(t)
			var f *fleet
			if disabled {
				f = startFleet(t)
			} else {
				f = tracedFleet(t, collector, 0, "0")
			}
			response, body := f.call(http.MethodPost, "/v1/chat/completions", f.key,
				`{"model":"qwen3.8-27b","max_tokens":2,"messages":[{"role":"user","content":"hello"}]}`)
			var result struct {
				Choices []json.RawMessage `json:"choices"`
			}
			if response.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &result) != nil || len(result.Choices) != 1 {
				t.Fatalf("non-recording path changed output: %d %s", response.StatusCode, body)
			}
			frames, posts := collector.Snapshot(t)
			if posts != 0 || len(frames) != 0 {
				t.Fatal("disabled or zero-sampled serving contacted the collector")
			}
		})
	}
}

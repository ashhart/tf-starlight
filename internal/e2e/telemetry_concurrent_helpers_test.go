package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/config"
	"github.com/ashhart/tf-starlight/internal/telemetry"
	"github.com/ashhart/tf-starlight/internal/testutil/tracecollector"
)

func concurrentTraceFleet(t *testing.T, collector *tracecollector.Collector, enabled bool, rate, queue, batch int) *fleet {
	t.Helper()
	f := startStarlight(t, func(c *config.Config) {
		c.Metrics.StreamTimings = true
		c.Database.MaxConns = 64
		if enabled {
			c.Telemetry = config.Telemetry{Endpoint: collector.URL(), SamplePerMillion: rate, Queue: queue,
				Batch: batch, Timeout: config.Duration{Duration: 5 * time.Second}}
		}
	})
	exec(t, f.pool, "UPDATE deployments SET spec=jsonb_set(spec,'{parallel}','32')")
	opts := nodeOptions{env: []string{"FAKE_ENGINE_DELAY_MS=0"}}
	if enabled {
		opts.telemetry = telemetry.Options{Endpoint: collector.URL(), SamplePerMillion: rate, Queue: queue, Batch: batch,
			Timeout: 5 * time.Second}
	}
	f.enrolWith(f.nodeURL, "concurrent-trace-node", 1, opts)
	return f
}

type concurrentTraceReply struct {
	elapsed float64
	output  string
	err     error
}

func concurrentTraceRequest(ctx context.Context, client *http.Client, f *fleet) concurrentTraceReply {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.public.URL+"/v1/chat/completions", strings.NewReader(
		`{"model":"qwen3.8-27b","max_tokens":8,"messages":[{"role":"user","content":"concurrent-private-prompt"}]}`))
	if err != nil {
		return concurrentTraceReply{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+f.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Baggage", "concurrent-private-identity")
	start := time.Now()
	response, err := client.Do(req)
	if err != nil {
		return concurrentTraceReply{err: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	elapsed := time.Since(start).Seconds()
	var result struct {
		Choices json.RawMessage `json:"choices"`
		Usage   json.RawMessage `json:"usage"`
	}
	if err != nil || response.StatusCode != http.StatusOK || response.Header.Get("X-Starlight-Route") != "local" ||
		json.Unmarshal(body, &result) != nil || len(result.Choices) == 0 || len(result.Usage) == 0 {
		return concurrentTraceReply{err: fmt.Errorf("concurrent fixture reply status=%d read=%v", response.StatusCode, err)}
	}
	output, err := json.Marshal(result)
	return concurrentTraceReply{elapsed: elapsed, output: string(output), err: err}
}

func concurrentTraceBurst(t *testing.T, f *fleet, count, workers int, expected *string) ([]float64, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	transport := &http.Transport{MaxConnsPerHost: workers, MaxIdleConnsPerHost: workers}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	jobs, replies := make(chan struct{}), make(chan concurrentTraceReply, count)
	var done sync.WaitGroup
	for range workers {
		done.Go(func() {
			for range jobs {
				replies <- concurrentTraceRequest(ctx, client, f)
			}
		})
	}
	start := time.Now()
	for range count {
		jobs <- struct{}{}
	}
	close(jobs)
	done.Wait()
	elapsed := time.Since(start)
	close(replies)
	var samples []float64
	for reply := range replies {
		if reply.err != nil {
			t.Fatal(reply.err)
		}
		if *expected == "" {
			*expected = reply.output
		}
		if reply.output != *expected {
			t.Fatal("enabled telemetry changed choices or observed usage")
		}
		samples = append(samples, reply.elapsed)
	}
	if len(samples) != count {
		t.Fatal("concurrent profile lost completed requests")
	}
	return samples, elapsed
}

func concurrentTraceHealth(t *testing.T, f *fleet) map[string]float64 {
	t.Helper()
	families, err := f.starlight.Metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	health := map[string]float64{}
	for _, family := range families {
		if strings.HasPrefix(family.GetName(), "starlight_phase_telemetry_") {
			for _, metric := range family.Metric {
				health[family.GetName()] += metric.GetCounter().GetValue() + metric.GetGauge().GetValue()
			}
		}
	}
	return health
}

func concurrentTraceAccounting(t *testing.T, f *fleet, count int) {
	t.Helper()
	var events, leases int
	if err := f.pool.QueryRow(f.ctx, "SELECT (SELECT count(*) FROM usage_events),(SELECT count(*) FROM leases)").
		Scan(&events, &leases); err != nil || events != count || leases != 0 {
		t.Fatalf("concurrent tracing changed accounting: events=%d leases=%d: %v", events, leases, err)
	}
}

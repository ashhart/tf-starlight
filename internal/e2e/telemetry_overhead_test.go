package e2e_test

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/testutil/tracecollector"
)

func TestPhaseTelemetryOverhead(t *testing.T) {
	if os.Getenv("STARLIGHT_PHASE_TELEMETRY_OVERHEAD") != "1" {
		t.Skip("set STARLIGHT_PHASE_TELEMETRY_OVERHEAD=1 for matched fake-engine telemetry profiles")
	}
	for trial, mode := range []string{"disabled", "sampled", "full", "full", "sampled", "disabled"} {
		t.Run(mode+"-"+string(rune('a'+trial)), func(t *testing.T) {
			collector := tracecollector.New(t)
			var f *fleet
			rate := 0
			if mode == "disabled" {
				f = startStarlight(t, nil)
				f.enrolWith(f.nodeURL, "profile-node", 1, nodeOptions{env: []string{"FAKE_ENGINE_DELAY_MS=0"}})
			} else {
				rate = 10000
				if mode == "full" {
					rate = 1000000
				}
				f = tracedFleet(t, collector, rate, "0")
			}
			const warmup, count = 30, 300
			var samples []float64
			for i := range warmup + count {
				start := time.Now()
				response, _ := f.call(http.MethodPost, "/v1/chat/completions", f.key,
					`{"model":"qwen3.8-27b","max_tokens":2,"messages":[{"role":"user","content":"profile fixture"}]}`)
				elapsed := time.Since(start).Seconds()
				if response.StatusCode != http.StatusOK {
					t.Fatalf("profile request failed: %d", response.StatusCode)
				}
				if i >= warmup {
					samples = append(samples, elapsed)
				}
			}
			deadline := time.Now().Add(3 * time.Second)
			previous, stable := -1, 0
			var frames []tracecollector.Frame
			var posts int
			for stable < 5 && time.Now().Before(deadline) {
				time.Sleep(100 * time.Millisecond)
				frames, posts = collector.Snapshot(t)
				if len(frames) == previous {
					stable++
				} else {
					previous, stable = len(frames), 0
				}
			}
			traces := map[string]int{}
			for _, frame := range frames {
				traces[frame.Trace]++
			}
			complete := 0
			for _, spans := range traces {
				if spans == 11 {
					complete++
				}
			}
			if mode == "disabled" && (posts != 0 || len(frames) != 0) {
				t.Fatal("disabled profile contacted the exporter")
			}
			slices.Sort(samples)
			var total float64
			for _, value := range samples {
				total += value
			}
			report := map[string]any{"basis": "single-host sequential fake-engine profiles; not device or production-load evidence",
				"mode": mode, "trial": trial, "sampling_per_million": rate, "samples": count, "warmup": warmup,
				"p50_seconds": samples[int(math.Ceil(float64(count)*0.5))-1],
				"p95_seconds": samples[int(math.Ceil(float64(count)*0.95))-1], "mean_seconds": total / count,
				"collector_posts": posts, "accepted_spans": len(frames), "observed_traces": len(traces), "complete_traces": complete,
				"expected_spans_per_full_json_request": 11, "captured_after_quiescence": stable >= 5}
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			t.Log("phase_telemetry_profile " + string(encoded))
		})
	}
}

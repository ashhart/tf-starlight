package e2e_test

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/testutil/tracecollector"
)

func TestConcurrentPhaseTelemetryProfiles(t *testing.T) {
	if os.Getenv("STARLIGHT_PHASE_TELEMETRY_CONCURRENT") != "1" {
		t.Skip("set STARLIGHT_PHASE_TELEMETRY_CONCURRENT=1 for concurrent fake-engine export profiles")
	}
	var expected string
	for trial, mode := range []string{"disabled", "sampled", "full", "full", "sampled", "disabled"} {
		t.Run(fmt.Sprintf("%d-%s", trial, mode), func(t *testing.T) {
			collector := tracecollector.New(t)
			rate := 10000
			if mode == "full" {
				rate = 1000000
			}
			if mode == "disabled" {
				rate = 0
			}
			f := concurrentTraceFleet(t, collector, mode != "disabled", rate, 256, 32)
			const warmup, samples, workers = 16, 256, 16
			concurrentTraceBurst(t, f, warmup, 1, &expected)
			times, elapsed := concurrentTraceBurst(t, f, samples, workers, &expected)
			frames, posts, quiet := concurrentTraceSnapshot(t, collector)
			concurrentTraceAccounting(t, f, warmup+samples)
			health := concurrentTraceHealth(t, f)
			if health["starlight_phase_telemetry_queue"] > 256 ||
				mode == "disabled" && (posts != 0 || len(frames) != 0 || len(health) != 0) {
				t.Fatal("export configuration or queue bound was not respected")
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
			collector.AssertPrivate(t, "concurrent-private-prompt", "concurrent-private-identity", f.key, f.project, f.user.ID, f.model)
			slices.Sort(times)
			concurrentTraceReport(t, "phase_telemetry_concurrent_profile", map[string]any{
				"basis": "single-host fake-engine concurrent HTTP; no real-device or production SLO",
				"mode":  mode, "trial": trial, "workers": workers, "samples": samples, "warmup": warmup,
				"queue": 256, "batch": 32, "sampling_per_million": rate,
				"p50_seconds": times[(len(times)+1)/2-1], "p95_seconds": times[(len(times)*95+99)/100-1],
				"wall_seconds": elapsed.Seconds(), "requests_per_second": samples / elapsed.Seconds(),
				"collector_posts": posts, "accepted_spans": len(frames), "observed_traces": len(traces),
				"complete_traces": complete, "captured_after_quiescence": quiet, "gateway_exporter": health,
				"choices_and_usage_equal": true, "usage_rows": warmup + samples, "leases": 0})
		})
	}
}

func TestConcurrentPhaseTelemetryDoesNotWaitForCollector(t *testing.T) {
	if os.Getenv("STARLIGHT_PHASE_TELEMETRY_CONCURRENT") != "1" {
		t.Skip("set STARLIGHT_PHASE_TELEMETRY_CONCURRENT=1 for held-collector inference qualification")
	}
	collector, release := tracecollector.NewBlocked(t)
	f := concurrentTraceFleet(t, collector, true, 1000000, 4, 1)
	var expected string
	concurrentTraceBurst(t, f, 1, 1, &expected)
	const samples, workers = 256, 16
	_, elapsed := concurrentTraceBurst(t, f, samples, workers, &expected)
	frames, posts := collector.Snapshot(t)
	health := concurrentTraceHealth(t, f)
	if posts == 0 || len(frames) == 0 || health["starlight_phase_telemetry_requests_total"] < 1 ||
		health["starlight_phase_telemetry_sent_total"] != 0 || health["starlight_phase_telemetry_queue_full_total"] == 0 ||
		health["starlight_phase_telemetry_queue"] > 4 {
		t.Fatalf("held collector did not exercise bounded pending loss: posts=%d health=%v", posts, health)
	}
	concurrentTraceAccounting(t, f, samples+1)
	concurrentTraceReport(t, "phase_telemetry_held_collector", map[string]any{
		"workers": workers, "completed_while_responses_held": samples, "wall_seconds": elapsed.Seconds(),
		"gateway_exporter_before_release": health, "queue_bound": 4, "accepted_frames_before_release": len(frames),
		"choices_and_usage_equal": true, "usage_rows_before_release": samples + 1, "leases": 0})
	release()
	concurrentTraceBurst(t, f, 1, 1, &expected)
	deadline := time.Now().Add(3 * time.Second)
	for concurrentTraceHealth(t, f)["starlight_phase_telemetry_sent_total"] == 0 {
		if time.Now().After(deadline) {
			t.Fatal("exporter did not accept work after collector release")
		}
		time.Sleep(5 * time.Millisecond)
	}
	concurrentTraceAccounting(t, f, samples+2)
	collector.AssertPrivate(t, "concurrent-private-prompt", "concurrent-private-identity", f.key, f.project, f.user.ID, f.model)
}

func concurrentTraceSnapshot(t *testing.T, collector *tracecollector.Collector) ([]tracecollector.Frame, int, bool) {
	t.Helper()
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
	return frames, posts, stable >= 5
}

func concurrentTraceReport(t *testing.T, name string, report map[string]any) {
	t.Helper()
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(name + " " + string(b))
}

package e2e_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/config"
)

func TestStreamTimingsAreOptInOnTheGovernedFleetPath(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			f := startStarlight(t, func(c *config.Config) { c.Metrics.StreamTimings = enabled })
			f.enrolNode(f.nodeURL)
			resp, body := f.call("POST", "/v1/chat/completions", f.key,
				`{"model":"qwen3.8-27b","stream":true,"max_tokens":4,"messages":[{"role":"user","content":"hi"}]}`)
			if resp.StatusCode != 200 || !strings.Contains(body, "data: [DONE]") {
				t.Fatalf("stream returned %d: %s", resp.StatusCode, body)
			}
			families, err := f.starlight.Metrics.Registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]uint64{}
			for _, family := range families {
				for _, metric := range family.Metric {
					if metric.Histogram != nil {
						counts[family.GetName()] += metric.Histogram.GetSampleCount()
					}
				}
			}
			if counts["starlight_first_byte_seconds"] != 1 {
				t.Fatal("legacy first-byte observation changed")
			}
			for _, series := range []string{"starlight_upstream_first_data_seconds", "starlight_first_model_output_seconds",
				"starlight_first_model_output_flush_seconds"} {
				want := uint64(0)
				if enabled {
					want = 1
				}
				if counts[series] != want {
					t.Errorf("%s count %d, want %d", series, counts[series], want)
				}
			}
		})
	}
}

func TestTimedOutStreamObservationsUseTheFinalOutcome(t *testing.T) {
	f := startStarlight(t, func(c *config.Config) {
		c.Metrics.StreamTimings = true
		c.Gateway.RequestTimeout.Duration = 2 * time.Second
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	f.addFrontier(&frontier{srv: upstream})
	response, _ := f.call("POST", "/v1/chat/completions", f.key,
		`{"model":"frontier-planner","stream":true,"max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`)
	if response.StatusCode != 200 {
		t.Fatalf("stream returned %d", response.StatusCode)
	}
	families, err := f.starlight.Metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	wanted := map[string]bool{"starlight_upstream_first_data_seconds": true, "starlight_first_model_output_seconds": true,
		"starlight_first_model_output_flush_seconds": true}
	for _, family := range families {
		if !wanted[family.GetName()] {
			continue
		}
		delete(wanted, family.GetName())
		if len(family.Metric) != 1 || family.Metric[0].Histogram.GetSampleCount() != 1 {
			t.Fatal("missing stream observation")
		}
		outcome := ""
		for _, label := range family.Metric[0].Label {
			if label.GetName() == "outcome" {
				outcome = label.GetValue()
			}
		}
		if outcome != "timeout" {
			t.Fatalf("%s outcome %q, want timeout", family.GetName(), outcome)
		}
	}
	if len(wanted) != 0 {
		t.Fatal("timed-out stream observations were not exported")
	}
}

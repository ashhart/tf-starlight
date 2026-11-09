package e2e_test

import (
	"strings"
	"testing"

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

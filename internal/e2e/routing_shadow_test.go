package e2e_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ashhart/tf-starlight/internal/config"
)

func TestRoutingShadowIsDefaultOffAndObservesTheActualGatewayPath(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			f := startStarlight(t, func(c *config.Config) { c.Gateway.RoutingShadow = enabled })
			f.enrolNode(f.nodeURL)
			response, body := f.call(http.MethodPost, "/v1/chat/completions", f.key,
				`{"model":"qwen3.8-27b","max_tokens":8,"messages":[{"role":"user","content":"shadow-private-prompt"}]}`)
			var result struct {
				Choices []json.RawMessage `json:"choices"`
				Usage   struct {
					Prompt     int64 `json:"prompt_tokens"`
					Completion int64 `json:"completion_tokens"`
				} `json:"usage"`
			}
			if response.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &result) != nil ||
				len(result.Choices) != 1 || result.Usage.Prompt+result.Usage.Completion <= 0 {
				t.Fatalf("shadow=%v changed normal serving: status=%d body=%s", enabled, response.StatusCode, body)
			}
			families, err := f.starlight.Metrics.Registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			var decisions float64
			for _, family := range families {
				if strings.HasPrefix(family.GetName(), "starlight_routing_shadow_") {
					for _, metric := range family.Metric {
						if family.GetName() == "starlight_routing_shadow_decisions_total" {
							decisions += metric.GetCounter().GetValue()
						}
						for _, label := range metric.Label {
							if strings.Contains(label.GetValue(), "shadow-private-prompt") ||
								label.GetName() == "replica" || label.GetName() == "request" {
								t.Fatal("shadow metrics exported prompt or unbounded identity labels")
							}
						}
					}
				}
			}
			want := 0.0
			if enabled {
				want = 1
			}
			if decisions != want {
				t.Fatalf("shadow=%v decisions=%v want=%v", enabled, decisions, want)
			}
		})
	}
}

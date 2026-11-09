package gateway

import (
	"context"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/httpx"
	"github.com/ashhart/tf-starlight/internal/metrics"
	"github.com/ashhart/tf-starlight/internal/providers"
)

func TestStreamObservationBudgetLeavesUnmatchedOutputUnknownAndPreservesBytes(t *testing.T) {
	output := "data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n"
	for name, prefix := range map[string]string{
		"event cap":      strings.Repeat("data: {\"unknown\":true}\n\n", maxOutputInspectionEvents),
		"byte cap":       "data: {\"unknown\":\"" + strings.Repeat("x", maxOutputInspectionBytes) + "\"}\n\n",
		"terminal event": "data: [DONE]\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			body := prefix + output
			times := streamTimes{start: time.Now()}
			w := httptest.NewRecorder()
			stream(context.Background(), w, strings.NewReader(body), &meter{protocol: providers.ChatCompletions}, false, httpx.OpenAI, &times)
			if times.output.seen || !times.stopped || times.examinedBytes > maxOutputInspectionBytes || times.events > maxOutputInspectionEvents {
				t.Fatalf("unbounded or invented observation: %+v", times)
			}
			if w.Body.String() != body {
				t.Fatal("inspection budget changed forwarded bytes")
			}
		})
	}
}

func TestStreamObservationMetricLabelNamesAndValuesAreFixed(t *testing.T) {
	g := &Gateway{Metrics: metrics.New()}
	for i, kind := range []payloadKind{textPayload, reasoningPayload, toolPayload} {
		g.observeStream("catalogue-model", "local", "ok", &streamTimes{data: seenTime{seen: true}, output: seenTime{seen: true},
			flush: seenTime{seen: true}, kind: kind, flushError: i == 1})
	}
	families, err := g.Metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		"starlight_upstream_first_data_seconds":      "model,outcome,route",
		"starlight_first_model_output_seconds":       "model,outcome,payload,route",
		"starlight_first_model_output_flush_seconds": "model,outcome,result,route",
	}
	payloads, results := map[string]bool{}, map[string]bool{}
	for _, family := range families {
		want, ok := expected[family.GetName()]
		if !ok {
			continue
		}
		delete(expected, family.GetName())
		for _, sample := range family.Metric {
			var names []string
			for _, label := range sample.Label {
				names = append(names, label.GetName())
				switch label.GetName() {
				case "payload":
					payloads[label.GetValue()] = true
				case "result":
					results[label.GetValue()] = true
				case "model":
					if label.GetValue() != "catalogue-model" {
						t.Fatal("unexpected model label")
					}
				case "route":
					if label.GetValue() != "local" {
						t.Fatal("unexpected route label")
					}
				case "outcome":
					if label.GetValue() != "ok" {
						t.Fatal("unexpected outcome label")
					}
				}
			}
			sort.Strings(names)
			if strings.Join(names, ",") != want {
				t.Fatalf("%s labels %v, want %s", family.GetName(), names, want)
			}
		}
	}
	if len(expected) != 0 || len(payloads) != 3 || !payloads["text"] || !payloads["reasoning"] || !payloads["tool"] ||
		len(results) != 2 || !results["ok"] || !results["error"] {
		t.Fatal("observation label vocabulary changed")
	}
}

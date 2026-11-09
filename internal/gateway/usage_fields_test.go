package gateway

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ashhart/tf-starlight/internal/providers"
)

func TestTokenCalibrationRequiresExplicitCompleteUsage(t *testing.T) {
	for _, tc := range []struct {
		name, usage        string
		prompt, completion int64
		known              bool
	}{
		{"explicit_zero", `{"prompt_tokens":0,"completion_tokens":0}`, 0, 0, true},
		{"short_reply", `{"prompt_tokens":4096,"completion_tokens":1}`, 4096, 1, true},
		{"wrong_pair", `{"input_tokens":512,"output_tokens":0}`, 512, 0, false},
		{"missing_usage", `null`, 0, 0, false},
		{"empty_usage", `{}`, 0, 0, false},
		{"missing_completion", `{"prompt_tokens":4096}`, 4096, 0, false},
		{"null_completion", `{"prompt_tokens":4096,"completion_tokens":null}`, 4096, 0, false},
		{"mixed_pair", `{"prompt_tokens":4,"output_tokens":3}`, 4, 3, false},
		{"both_pairs", `{"prompt_tokens":4,"completion_tokens":3,"input_tokens":4,"output_tokens":3}`, 8, 6, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := meter{protocol: providers.ChatCompletions, calibrateTokens: true}
			m.body([]byte(`{"usage":` + tc.usage + `}`))
			if m.usage.Prompt != tc.prompt || m.usage.Completion != tc.completion || m.usage.TokenCountsReported != tc.known {
				t.Fatalf("usage=%+v, want unchanged values %d/%d and known=%v", m.usage, tc.prompt, tc.completion, tc.known)
			}
		})
	}
}

func TestTokenCalibrationDoesNotChangeMeteringOrWireUsage(t *testing.T) {
	for _, raw := range []string{
		`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`,
		`{"usage":{"prompt_tokens":4096,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":512}}}`,
		`{"usage":{"prompt_tokens":9}}`, `{"usage":{}}`, `{"choices":[]}`,
	} {
		off := meter{protocol: providers.ChatCompletions}
		on := meter{protocol: providers.ChatCompletions, calibrateTokens: true}
		off.body([]byte(raw))
		on.body([]byte(raw))
		if off.usage.TokenCountsReported {
			t.Fatal("disabled calibration classified counts")
		}
		actual := on.usage
		actual.TokenCountsReported = false
		if !reflect.DeepEqual(off.usage, actual) {
			t.Fatalf("calibration changed metering: off=%+v on=%+v", off.usage, on.usage)
		}
		encoded, err := json.Marshal(on.usage)
		if err != nil || strings.Contains(string(encoded), "TokenCountsReported") {
			t.Fatal("calibration metadata entered wire usage")
		}
	}
}

func TestTokenCalibrationResponsesRequireCompletedExplicitCounts(t *testing.T) {
	for _, tc := range []struct {
		data  string
		known bool
	}{
		{`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":512,"output_tokens":0}}}`, true},
		{`{"type":"response.failed","response":{"status":"failed","usage":{"input_tokens":512,"output_tokens":1}}}`, false},
		{`{"type":"response.incomplete","response":{"usage":{"input_tokens":512,"output_tokens":1}}}`, false},
		{`{"type":"response.completed","response":{"usage":{"input_tokens":512}}}`, false},
	} {
		m := meter{protocol: providers.Responses, calibrateTokens: true}
		m.event([]byte(tc.data))
		if m.usage.TokenCountsReported != tc.known {
			t.Fatalf("response known=%v want=%v", m.usage.TokenCountsReported, tc.known)
		}
	}
	m := meter{protocol: providers.Responses, calibrateTokens: true}
	m.body([]byte(`{"status":"completed","usage":{"input_tokens":512,"output_tokens":0}}`))
	if !m.usage.TokenCountsReported || m.usage.Prompt != 512 || m.usage.Completion != 0 {
		t.Fatalf("complete response usage=%+v", m.usage)
	}
}

func TestTokenCalibrationDropsStaleMetadataAfterMalformedUsage(t *testing.T) {
	for _, body := range []bool{false, true} {
		m := meter{protocol: providers.ChatCompletions, calibrateTokens: true}
		valid := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":10}}`)
		malformed := []byte(`{"usage":{"prompt_tokens":4,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":"bad"}}}`)
		if body {
			m.body(valid)
		} else {
			m.event(valid)
		}
		if !m.usage.TokenCountsReported {
			t.Fatal("valid explicit usage was not classified")
		}
		if body {
			m.body(malformed)
		} else {
			m.event(malformed)
		}
		if m.usage.Prompt != 100 || m.usage.Completion != 10 || !m.usage.Observed || m.usage.TokenCountsReported {
			t.Fatalf("malformed frame changed ledger values or kept stale calibration: %+v", m.usage)
		}
	}
}

func TestTokenCalibrationRefusalSurvivesLaterUsage(t *testing.T) {
	for _, bad := range []string{
		`{"choices":42,"usage":{"prompt_tokens":20,"completion_tokens":2}}`,
		`{"usage":{"prompt_tokens":4,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":"bad"}}}`,
		`{"error":{"message":"failed"}}`,
		`{"usage":{"prompt_tokens":4},"usage":{"completion_tokens":3}}`,
		`{"usage":{"prompt_tokens":4,"prompt_tokens":5,"completion_tokens":3}}`,
		`{"usage":{"prompt_tokens":4},"Usage":{"completion_tokens":3}}`,
		`{"usage":{"prompt_tokens":4,"Prompt_Tokens":5,"completion_tokens":3}}`,
		`{"usage":{"prompt_tokens":4},"u\u017Fage":{"completion_tokens":3}}`,
	} {
		on := meter{protocol: providers.ChatCompletions, calibrateTokens: true}
		off := meter{protocol: providers.ChatCompletions}
		for _, frame := range []string{`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10}}`, bad,
			`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":1}}`} {
			on.event([]byte(frame))
			off.event([]byte(frame))
			u := on.usage
			u.TokenCountsReported = false
			if !reflect.DeepEqual(u, off.usage) {
				t.Fatal("calibration refusal changed accounting")
			}
		}
		if !on.calibrationFailed || on.usage.TokenCountsReported {
			t.Fatalf("later usage erased refusal for %s", bad)
		}
	}
}

func TestTokenCalibrationRejectsDuplicateAndContradictoryBodies(t *testing.T) {
	for _, tc := range []struct{ protocol, body string }{
		{providers.ChatCompletions, `{"usage":{"prompt_tokens":4},"usage":{"completion_tokens":3}}`},
		{providers.ChatCompletions, `{"usage":{"prompt_tokens":4,"completion_tokens":3,"completion_tokens":6}}`},
		{providers.ChatCompletions, `{"usage":{"prompt_tokens":4},"Usage":{"completion_tokens":3}}`},
		{providers.ChatCompletions, `{"usage":{"prompt_tokens":4,"Prompt_Tokens":5,"completion_tokens":3}}`},
		{providers.ChatCompletions, `{"usage":{"prompt_tokens":4},"u\u017Fage":{"completion_tokens":3}}`},
		{providers.Responses, `{"status":"completed","usage":{"input_tokens":4,"output_tokens":3},"response":{"usage":{"input_tokens":8,"output_tokens":7}}}`},
	} {
		on := meter{protocol: tc.protocol, calibrateTokens: true}
		off := meter{protocol: tc.protocol}
		on.body([]byte(tc.body))
		off.body([]byte(tc.body))
		u := on.usage
		u.TokenCountsReported = false
		if on.usage.TokenCountsReported || !on.calibrationFailed || !reflect.DeepEqual(u, off.usage) {
			t.Fatal("ambiguous body qualified or changed accounting")
		}
	}
	m := meter{protocol: providers.Responses, calibrateTokens: true}
	m.event([]byte(`{"type":"response.failed","response":{"usage":{"input_tokens":8,"output_tokens":1}}}`))
	m.event([]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":8,"output_tokens":2}}}`))
	if m.usage.TokenCountsReported || !m.calibrationFailed {
		t.Fatal("later complete response erased failed terminal")
	}
}

func TestTokenCalibrationJSONLimitsRemainUnknown(t *testing.T) {
	for _, data := range []string{
		`{"usage":{"prompt_tokens":1,"completion_tokens":1},"extra":"` + strings.Repeat("x", 1<<20) + `"}`,
		`{"extra":` + strings.Repeat("[", 65) + `0` + strings.Repeat("]", 65) + `}`,
		`{"extra":[` + strings.Repeat("0,", 4096) + `0]}`,
	} {
		if uniqueCalibrationJSON([]byte(data)) {
			t.Fatal("oversized diagnostic payload became a qualifying observation")
		}
	}
}

func TestTokenCalibrationProtocolEnvelope(t *testing.T) {
	for _, protocol := range []string{providers.ChatCompletions, providers.Completions, providers.Responses} {
		if !tokenEstimateProtocol(protocol) {
			t.Fatalf("generation protocol %s omitted", protocol)
		}
	}
	for _, protocol := range []string{providers.Messages, providers.CountTokens, providers.Embeddings, "unknown"} {
		if tokenEstimateProtocol(protocol) {
			t.Fatalf("unsupported protocol %s treated as complete", protocol)
		}
	}
}

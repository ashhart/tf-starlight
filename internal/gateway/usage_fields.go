package gateway

import (
	"encoding/json"

	"github.com/ashhart/tf-starlight/internal/observations"
	"github.com/ashhart/tf-starlight/internal/providers"
)

type reportedCounts struct {
	Prompt     *int64 `json:"prompt_tokens"`
	Completion *int64 `json:"completion_tokens"`
	Input      *int64 `json:"input_tokens"`
	Output     *int64 `json:"output_tokens"`
}

// classifyTokenCounts rejects ambiguous or failed observations without changing the existing accounting parser.
func classifyTokenCounts(protocol string, data []byte, stream bool) (complete, invalid bool) {
	if !tokenEstimateProtocol(protocol) || !uniqueCalibrationJSON(data) {
		return false, true
	}
	var envelope struct {
		Type     string            `json:"type"`
		Status   string            `json:"status"`
		Error    json.RawMessage   `json:"error"`
		Choices  []json.RawMessage `json:"choices"`
		Usage    *reportedCounts   `json:"usage"`
		Response *struct {
			Status string          `json:"status"`
			Error  json.RawMessage `json:"error"`
			Usage  *reportedCounts `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &envelope) != nil || reportedError(envelope.Error) ||
		envelope.Type == "error" || envelope.Type == "response.failed" || envelope.Type == "response.incomplete" ||
		envelope.Status == "failed" || envelope.Status == "incomplete" {
		return false, true
	}
	u := envelope.Usage
	if protocol == providers.Responses {
		if stream {
			if envelope.Usage != nil {
				return false, true
			}
			if envelope.Response == nil {
				return false, false
			}
			if reportedError(envelope.Response.Error) || envelope.Response.Status == "failed" || envelope.Response.Status == "incomplete" {
				return false, true
			}
			if envelope.Type != "response.completed" {
				return false, false
			}
			if envelope.Response.Status != "" && envelope.Response.Status != "completed" {
				return false, true
			}
			u = envelope.Response.Usage
		} else {
			if envelope.Response != nil {
				return false, true
			}
			if envelope.Type != "" || envelope.Status != "completed" {
				return false, false
			}
		}
	} else if envelope.Response != nil {
		return false, true
	}
	if u == nil {
		return false, false
	}
	for _, n := range []*int64{u.Prompt, u.Completion, u.Input, u.Output} {
		if n != nil && (*n < 0 || *n > observations.MaxCounter) {
			return false, true
		}
	}
	if protocol == providers.Responses {
		if u.Prompt != nil || u.Completion != nil {
			return false, true
		}
		return u.Input != nil && u.Output != nil, false
	}
	if u.Input != nil || u.Output != nil {
		return false, true
	}
	return u.Prompt != nil && u.Completion != nil, false
}

func reportedError(value json.RawMessage) bool {
	return len(value) != 0 && string(value) != "null"
}

func tokenEstimateProtocol(protocol string) bool {
	switch protocol {
	case providers.ChatCompletions, providers.Completions, providers.Responses:
		return true
	default:
		return false
	}
}

func (m *meter) observeTokenCounts(data []byte, stream bool) {
	if !m.calibrateTokens || m.calibrationFailed {
		return
	}
	complete, invalid := classifyTokenCounts(m.protocol, data, stream)
	m.calibrationFailed = invalid
	m.usage.TokenCountsReported = complete && !invalid
}

func (m *meter) failTokenCalibration() {
	if m.calibrateTokens {
		m.calibrationFailed = true
		m.usage.TokenCountsReported = false
	}
}

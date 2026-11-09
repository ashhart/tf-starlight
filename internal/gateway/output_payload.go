package gateway

import (
	"bytes"
	"encoding/json"

	"github.com/ashhart/tf-starlight/internal/providers"
)

type payloadKind string

const (
	textPayload      payloadKind = "text"
	reasoningPayload payloadKind = "reasoning"
	toolPayload      payloadKind = "tool"
)

type functionPayload struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (f functionPayload) present() bool { return f.Name != "" || f.Arguments != "" }

type choicePayload struct {
	Text  string `json:"text"`
	Delta struct {
		Content          string          `json:"content"`
		ReasoningContent string          `json:"reasoning_content"`
		Reasoning        string          `json:"reasoning"`
		Refusal          string          `json:"refusal"`
		FunctionCall     functionPayload `json:"function_call"`
		ToolCalls        []struct {
			Function functionPayload `json:"function"`
		} `json:"tool_calls"`
	} `json:"delta"`
}

type typedPayload struct {
	Type  string          `json:"type"`
	Error json.RawMessage `json:"error"`
	Delta json.RawMessage `json:"delta"`
	Item  struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"item"`
	Block struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"content_block"`
}

func messageHeartbeat(protocol string, data []byte) bool {
	if protocol != providers.Messages {
		return false
	}
	var event struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(data, &event) == nil && event.Type == "ping"
}

// modelPayload recognizes generated text, reasoning and tool payloads, never transport or lifecycle metadata.
func modelPayload(protocol string, data []byte) payloadKind {
	var envelope typedPayload
	if json.Unmarshal(data, &envelope) != nil || len(envelope.Error) > 0 && !bytes.Equal(envelope.Error, []byte("null")) {
		return ""
	}
	switch protocol {
	case providers.ChatCompletions, providers.Completions:
		var event struct {
			Choices []choicePayload `json:"choices"`
		}
		if json.Unmarshal(data, &event) != nil {
			return ""
		}
		for _, c := range event.Choices {
			if c.Text != "" || c.Delta.Content != "" || c.Delta.Refusal != "" {
				return textPayload
			}
			if c.Delta.ReasoningContent != "" || c.Delta.Reasoning != "" {
				return reasoningPayload
			}
			if c.Delta.FunctionCall.present() {
				return toolPayload
			}
			for _, call := range c.Delta.ToolCalls {
				if call.Function.present() {
					return toolPayload
				}
			}
		}
	case providers.Responses:
		var delta string
		_ = json.Unmarshal(envelope.Delta, &delta)
		switch envelope.Type {
		case "response.output_text.delta", "response.refusal.delta":
			if delta != "" {
				return textPayload
			}
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			if delta != "" {
				return reasoningPayload
			}
		case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.mcp_call_arguments.delta":
			if delta != "" {
				return toolPayload
			}
		case "response.output_item.added":
			if envelope.Item.Name != "" && (envelope.Item.Type == "function_call" || envelope.Item.Type == "custom_tool_call") {
				return toolPayload
			}
		}
	case providers.Messages:
		switch envelope.Type {
		case "content_block_start":
			switch {
			case envelope.Block.Type == "text" && envelope.Block.Text != "":
				return textPayload
			case envelope.Block.Type == "thinking" && envelope.Block.Thinking != "":
				return reasoningPayload
			case envelope.Block.Type == "tool_use" && envelope.Block.Name != "":
				return toolPayload
			}
		case "content_block_delta":
			var delta struct {
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			}
			if json.Unmarshal(envelope.Delta, &delta) == nil {
				switch {
				case delta.Text != "":
					return textPayload
				case delta.Thinking != "":
					return reasoningPayload
				case delta.PartialJSON != "":
					return toolPayload
				}
			}
		}
	}
	return ""
}

// eventData joins the data fields of one complete SSE event without modifying its forwarded bytes.
func eventData(block []byte) []byte {
	var fields [][]byte
	for _, line := range bytes.Split(block, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		field := line[5:]
		if len(field) > 0 && field[0] == ' ' {
			field = field[1:]
		}
		fields = append(fields, field)
	}
	if len(fields) == 1 {
		return fields[0]
	}
	return bytes.Join(fields, []byte("\n"))
}

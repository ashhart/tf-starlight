package gateway

import (
	"bytes"
	"testing"

	"github.com/ashhart/tf-starlight/internal/providers"
)

func TestModelPayloadSeparatesGeneratedOutputFromMetadata(t *testing.T) {
	cases := []struct {
		name, protocol, data string
		want                 payloadKind
	}{
		{"role", providers.ChatCompletions, `{"choices":[{"delta":{"role":"assistant"}}]}`, ""},
		{"usage", providers.ChatCompletions, `{"choices":[],"usage":{"completion_tokens":2}}`, ""},
		{"id", providers.ChatCompletions, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a"}]}}]}`, ""},
		{"finish", providers.ChatCompletions, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, ""},
		{"empty", providers.ChatCompletions, `{"choices":[{"delta":{"content":""}}]}`, ""},
		{"text", providers.ChatCompletions, `{"choices":[{"delta":{"content":"hi"}}]}`, textPayload},
		{"space", providers.ChatCompletions, `{"choices":[{"delta":{"content":" "}}]}`, textPayload},
		{"reasoning", providers.ChatCompletions, `{"choices":[{"delta":{"reasoning_content":"think"}}]}`, reasoningPayload},
		{"refusal", providers.ChatCompletions, `{"choices":[{"delta":{"refusal":"No"}}]}`, textPayload},
		{"tool name", providers.ChatCompletions, `{"choices":[{"delta":{"tool_calls":[{"function":{"name":"lookup"}}]}}]}`, toolPayload},
		{"tool fragment", providers.ChatCompletions, `{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{"}}]}}]}`, toolPayload},
		{"legacy function", providers.ChatCompletions, `{"choices":[{"delta":{"function_call":{"name":"lookup"}}}]}`, toolPayload},
		{"later choice", providers.ChatCompletions, `{"choices":[{"delta":{}},{"delta":{"content":"x"}}]}`, textPayload},
		{"completion", providers.Completions, `{"choices":[{"text":"ok"}]}`, textPayload},
		{"error", providers.ChatCompletions, `{"error":{"message":"failed"},"choices":[{"delta":{"content":"x"}}]}`, ""},
		{"null error", providers.ChatCompletions, `{"error":null,"choices":[{"delta":{"content":"x"}}]}`, textPayload},
		{"malformed", providers.ChatCompletions, `{"choices":[`, ""},
		{"wrong field type", providers.ChatCompletions, `{"choices":[{"delta":{"content":3}}]}`, ""},
		{"created", providers.Responses, `{"type":"response.created","response":{"id":"r"}}`, ""},
		{"response text", providers.Responses, `{"type":"response.output_text.delta","delta":"hi"}`, textPayload},
		{"response reasoning", providers.Responses, `{"type":"response.reasoning_text.delta","delta":"think"}`, reasoningPayload},
		{"response summary", providers.Responses, `{"type":"response.reasoning_summary_text.delta","delta":"think"}`, reasoningPayload},
		{"response tool", providers.Responses, `{"type":"response.function_call_arguments.delta","delta":"{"}`, toolPayload},
		{"custom tool", providers.Responses, `{"type":"response.custom_tool_call_input.delta","delta":"code"}`, toolPayload},
		{"mcp arguments", providers.Responses, `{"type":"response.mcp_call_arguments.delta","delta":"{"}`, toolPayload},
		{"response tool name", providers.Responses, `{"type":"response.output_item.added","item":{"type":"function_call","name":"lookup"}}`, toolPayload},
		{"response item id", providers.Responses, `{"type":"response.output_item.added","item":{"type":"function_call","id":"i"}}`, ""},
		{"response lifecycle", providers.Responses, `{"type":"response.completed","response":{"usage":{"output_tokens":1}}}`, ""},
		{"unknown delta", providers.Responses, `{"type":"response.unknown.delta","delta":"metadata"}`, ""},
		{"message start", providers.Messages, `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`, ""},
		{"anthropic text", providers.Messages, `{"type":"content_block_delta","delta":{"text":"hi"}}`, textPayload},
		{"anthropic reasoning", providers.Messages, `{"type":"content_block_delta","delta":{"thinking":"think"}}`, reasoningPayload},
		{"anthropic fragment", providers.Messages, `{"type":"content_block_delta","delta":{"partial_json":"{"}}`, toolPayload},
		{"anthropic tool name", providers.Messages, `{"type":"content_block_start","content_block":{"type":"tool_use","name":"lookup"}}`, toolPayload},
		{"anthropic initial text", providers.Messages, `{"type":"content_block_start","content_block":{"type":"text","text":"hi"}}`, textPayload},
		{"anthropic empty block", providers.Messages, `{"type":"content_block_start","content_block":{"type":"text","text":""}}`, ""},
		{"signature", providers.Messages, `{"type":"content_block_delta","delta":{"signature":"opaque"}}`, ""},
		{"unknown protocol", providers.Embeddings, `{"choices":[{"text":"x"}]}`, ""},
		{"done marker", providers.ChatCompletions, `[DONE]`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelPayload(tc.protocol, []byte(tc.data)); got != tc.want {
				t.Fatalf("payload kind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEventDataFollowsSSEFramingWithoutChangingWireBytes(t *testing.T) {
	cases := []struct{ block, want string }{
		{"event: update\r\ndata: one\r\n\r\n", "one"},
		{"data: {\"choices\":\ndata: [{\"text\":\"hi\"}]}\n\n", "{\"choices\":\n[{\"text\":\"hi\"}]}"},
		{": comment\nid: data: ignored\ndata:  two spaces\n\n", " two spaces"},
		{"data:\ndata: x\ndata:\n\n", "\nx\n"},
		{"data:x\n\n", "x"},
		{": ping\n\n", ""},
	}
	for _, tc := range cases {
		block := []byte(tc.block)
		before := bytes.Clone(block)
		if got := string(eventData(block)); got != tc.want {
			t.Errorf("eventData(%q) = %q, want %q", tc.block, got, tc.want)
		}
		if !bytes.Equal(before, block) {
			t.Fatal("observer modified the forwarded block")
		}
	}
}

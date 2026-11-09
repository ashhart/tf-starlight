package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/ashhart/tf-starlight/internal/providers"
)

func validateStreamGateEndpoint(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") ||
		(u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") {
		return errors.New("stream gate requires a loopback HTTP endpoint without URL credentials")
	}
	return nil
}

// clientGateData implements fixture SSE framing independently of the relay's eventData helper.
func clientGateData(block []byte) []byte {
	var fields []string
	scanner := bufio.NewScanner(bytes.NewReader(block))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.HasPrefix(line, "data:") {
			field := strings.TrimPrefix(line, "data:")
			fields = append(fields, strings.TrimPrefix(field, " "))
		}
	}
	return []byte(strings.Join(fields, "\n"))
}

func clientObject(v any) map[string]any { m, _ := v.(map[string]any); return m }
func clientText(v any) bool             { s, ok := v.(string); return ok && s != "" }

// clientGatePayload is a separate map-based fixture decoder, without the production recognizer or its structs.
func clientGatePayload(protocol string, data []byte) (kind string, tool bool) {
	var event map[string]any
	if json.Unmarshal(data, &event) != nil || event["error"] != nil {
		return "", false
	}
	function := func(v any) bool { m := clientObject(v); return clientText(m["name"]) || clientText(m["arguments"]) }
	switch protocol {
	case providers.ChatCompletions:
		choices, _ := event["choices"].([]any)
		for _, v := range choices {
			choice := clientObject(v)
			delta := clientObject(choice["delta"])
			current := ""
			if clientText(choice["text"]) || clientText(delta["content"]) || clientText(delta["refusal"]) {
				current = "text"
			} else if clientText(delta["reasoning_content"]) || clientText(delta["reasoning"]) {
				current = "reasoning"
			}
			call := function(delta["function_call"])
			calls, _ := delta["tool_calls"].([]any)
			for _, v := range calls {
				call = call || function(clientObject(v)["function"])
			}
			if call {
				tool = true
				if current == "" {
					current = "tool"
				}
			}
			if kind == "" {
				kind = current
			}
		}
	case providers.Responses:
		typeName, _ := event["type"].(string)
		switch typeName {
		case "response.output_text.delta", "response.refusal.delta":
			if clientText(event["delta"]) {
				kind = "text"
			}
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			if clientText(event["delta"]) {
				kind = "reasoning"
			}
		case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.mcp_call_arguments.delta":
			tool = clientText(event["delta"])
		case "response.output_item.added":
			item := clientObject(event["item"])
			tool = (item["type"] == "function_call" || item["type"] == "custom_tool_call") && clientText(item["name"])
		}
	case providers.Messages:
		if event["type"] == "content_block_start" {
			block := clientObject(event["content_block"])
			switch block["type"] {
			case "text":
				if clientText(block["text"]) {
					kind = "text"
				}
			case "thinking":
				if clientText(block["thinking"]) {
					kind = "reasoning"
				}
			case "tool_use":
				tool = clientText(block["name"])
			}
		}
		if event["type"] == "content_block_delta" {
			delta := clientObject(event["delta"])
			switch delta["type"] {
			case "text_delta":
				if clientText(delta["text"]) {
					kind = "text"
				}
			case "thinking_delta":
				if clientText(delta["thinking"]) {
					kind = "reasoning"
				}
			case "input_json_delta":
				tool = clientText(delta["partial_json"])
			}
		}
	}
	if kind == "" && tool {
		kind = "tool"
	}
	return kind, tool
}

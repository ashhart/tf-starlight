package mcpgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// response validates one upstream reply and filters tool discovery to the configured authority.
func response(raw []byte, expected request, allowed map[string]bool, maxBytes int, credential string) ([]byte, bool, error) {
	if checkJSONLimit(raw, maxBytes) != nil {
		return nil, false, errors.New("MCP upstream returned invalid JSON")
	}
	if reflectsCredential(raw, credential) {
		return nil, false, errors.New("MCP upstream response contains a protected credential")
	}
	var message map[string]json.RawMessage
	if json.Unmarshal(raw, &message) != nil || message == nil {
		return nil, false, errors.New("MCP upstream returned a non-object message")
	}
	for key := range message {
		if key != "jsonrpc" && key != "id" && key != "result" && key != "error" && key != "method" && key != "params" {
			return nil, false, errors.New("MCP upstream returned an ambiguous envelope")
		}
	}
	var version string
	if json.Unmarshal(message["jsonrpc"], &version) != nil || version != "2.0" {
		return nil, false, errors.New("MCP upstream returned an invalid RPC envelope")
	}
	if message["method"] != nil {
		var method string
		if message["id"] != nil || message["result"] != nil || message["error"] != nil ||
			json.Unmarshal(message["method"], &method) != nil || method != "notifications/progress" ||
			!validProgress(message["params"], expected.progress) {
			return nil, false, errors.New("MCP upstream returned an unsupported server message")
		}
		return raw, false, nil
	}
	if !sameID(expected.ID, message["id"]) || (message["result"] == nil) == (message["error"] == nil) {
		return nil, false, errors.New("MCP upstream returned a mismatched or ambiguous response")
	}
	if message["params"] != nil {
		return nil, false, errors.New("MCP upstream mixed response and notification fields")
	}
	if message["error"] != nil {
		var fields map[string]json.RawMessage
		if json.Unmarshal(message["error"], &fields) != nil || fieldAliases(fields, "code", "message", "data") {
			return nil, false, errors.New("MCP upstream returned ambiguous error fields")
		}
		var problem struct {
			Code    *int   `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(message["error"], &problem) != nil || problem.Code == nil || problem.Message == "" {
			return nil, false, errors.New("MCP upstream returned an invalid error")
		}
		return raw, true, nil
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(message["result"], &result) != nil || result == nil {
		return nil, false, errors.New("MCP upstream returned an invalid result")
	}
	if fieldAliases(result, "resultType", "inputRequests", "requestState", "tools", "cacheScope", "ttlMs", "task", "content", "isError", "nextCursor") {
		return nil, false, errors.New("MCP upstream returned ambiguous result fields")
	}
	var kind string
	if json.Unmarshal(result["resultType"], &kind) != nil || kind != "complete" || result["inputRequests"] != nil || result["requestState"] != nil || result["task"] != nil {
		return nil, false, errors.New("MCP upstream result is outside this transport profile")
	}
	if expected.Method != "tools/list" {
		var content []json.RawMessage
		if json.Unmarshal(result["content"], &content) != nil || content == nil {
			return nil, false, errors.New("MCP tool result requires content")
		}
		if raw, ok := result["isError"]; ok {
			var flag bool
			if string(raw) == "null" || json.Unmarshal(raw, &flag) != nil {
				return nil, false, errors.New("MCP tool result has invalid error flag")
			}
		}
		return raw, true, nil
	}
	var tools []json.RawMessage
	if json.Unmarshal(result["tools"], &tools) != nil || tools == nil || len(tools) > 1024 {
		return nil, false, errors.New("MCP upstream returned an invalid tool list")
	}
	if cursor, ok := result["nextCursor"]; ok {
		var value string
		if string(cursor) == "null" || json.Unmarshal(cursor, &value) != nil || len(value) > 4096 {
			return nil, false, errors.New("MCP upstream cursor is outside its bound")
		}
	}
	type named struct {
		name string
		raw  json.RawMessage
	}
	filtered := []named{}
	seen := map[string]bool{}
	for _, raw := range tools {
		var tool map[string]json.RawMessage
		var name string
		if json.Unmarshal(raw, &tool) != nil || fieldAliases(tool, "name", "inputSchema", "outputSchema") ||
			json.Unmarshal(tool["name"], &name) != nil || name == "" || seen[name] {
			return nil, false, errors.New("MCP upstream returned ambiguous tool names")
		}
		seen[name] = true
		if !allowed[name] {
			continue
		}
		schema := tool["inputSchema"]
		if len(schema) == 0 || bytes.TrimSpace(schema)[0] != '{' {
			return nil, false, errors.New("MCP upstream returned an invalid tool schema")
		}
		if !validHeaderSchema(schema) {
			continue
		}
		filtered = append(filtered, named{name, raw})
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].name < filtered[j].name })
	visible := make([]json.RawMessage, 0, len(filtered))
	for _, tool := range filtered {
		visible = append(visible, tool.raw)
	}
	result["tools"], _ = encodeJSON(visible)
	result["cacheScope"], result["ttlMs"] = json.RawMessage(`"private"`), json.RawMessage(`0`)
	message["result"], _ = encodeJSON(result)
	output, err := encodeJSON(message)
	if len(output) > maxBytes {
		return nil, false, errors.New("MCP filtered result exceeds its bound")
	}
	return output, true, err
}

func fieldAliases(fields map[string]json.RawMessage, reserved ...string) bool {
	for key := range fields {
		for _, name := range reserved {
			if key != name && strings.EqualFold(key, name) {
				return true
			}
		}
	}
	return false
}

func encodeJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(value)
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), err
}

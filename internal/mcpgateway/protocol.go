// Package mcpgateway implements a bounded, explicitly configured MCP HTTP intermediary.
package mcpgateway

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"math/big"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

const protocolVersion = "2026-07-28"

type request struct {
	JSONRPC  string          `json:"jsonrpc"`
	ID       json.RawMessage `json:"id"`
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params"`
	name     string
	progress json.RawMessage
}

type failure struct {
	status  int
	code    int
	message string
}

func invalid() *failure { return &failure{400, -32600, "Invalid request"} }

func headerMismatch() *failure {
	return &failure{400, -32020, "MCP request headers do not match the body"}
}

func parseRequest(headers http.Header, raw []byte) (request, *failure) {
	var r request
	bytes, count := 0, 0
	for key, values := range headers {
		if !strings.HasPrefix(strings.ToLower(key), "mcp-") {
			continue
		}
		for _, value := range values {
			bytes += len(key) + len(value)
			count++
			if len(value) > 8192 || bytes > 16384 || count > 64 {
				return r, invalid()
			}
		}
	}
	if checkJSON(raw) != nil || json.Unmarshal(raw, &r) != nil || r.JSONRPC != "2.0" || !validID(r.ID) {
		return r, invalid()
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil || top["result"] != nil || top["error"] != nil {
		return r, invalid()
	}
	for key := range top {
		if key != "jsonrpc" && key != "id" && key != "method" && key != "params" {
			return r, invalid()
		}
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(r.Params, &params) != nil || params == nil {
		return r, invalid()
	}
	if len(headers.Values("Mcp-Session-Id")) != 0 || len(headers.Values("Last-Event-ID")) != 0 {
		return r, &failure{400, -32600, "Sessions and stream resumption are outside this transport profile"}
	}
	var meta map[string]json.RawMessage
	if json.Unmarshal(params["_meta"], &meta) != nil || meta == nil {
		return r, invalid()
	}
	if meta["io.modelcontextprotocol/logLevel"] != nil || meta["io.modelcontextprotocol/related-task"] != nil {
		return r, &failure{400, -32602, "Logging and tasks are outside this transport profile"}
	}
	if token, ok := meta["progressToken"]; ok {
		if !validID(token) {
			return r, &failure{400, -32602, "Progress token is outside the supported bounds"}
		}
		r.progress = token
	}
	var version string
	if json.Unmarshal(meta["io.modelcontextprotocol/protocolVersion"], &version) != nil || len(version) > 32 {
		return r, invalid()
	}
	var info struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	var capabilities map[string]json.RawMessage
	var infoFields map[string]json.RawMessage
	if json.Unmarshal(meta["io.modelcontextprotocol/clientInfo"], &infoFields) != nil || fieldAliases(infoFields, "name", "version") {
		return r, invalid()
	}
	if json.Unmarshal(meta["io.modelcontextprotocol/clientInfo"], &info) != nil || info.Name == "" || info.Version == "" ||
		len(info.Name) > 128 || len(info.Version) > 128 ||
		json.Unmarshal(meta["io.modelcontextprotocol/clientCapabilities"], &capabilities) != nil || capabilities == nil {
		return r, invalid()
	}
	if !singleHeader(headers, "MCP-Protocol-Version", version) || !singleHeader(headers, "Mcp-Method", r.Method) {
		return r, headerMismatch()
	}
	if version != protocolVersion {
		return r, &failure{400, -32022, "Unsupported MCP protocol version"}
	}
	if !accepts(headers, "application/json") || !accepts(headers, "text/event-stream") {
		return r, &failure{406, -32600, "Accept must include JSON and event streams"}
	}
	contentType, _, err := mime.ParseMediaType(headers.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return r, &failure{415, -32600, "Content-Type must be application/json"}
	}
	switch r.Method {
	case "server/discover", "tools/list":
		if len(headers.Values("Mcp-Name")) != 0 {
			return r, headerMismatch()
		}
	case "tools/call":
		if json.Unmarshal(params["name"], &r.name) != nil || r.name == "" || len(r.name) > 128 {
			return r, invalid()
		}
		values := headers.Values("Mcp-Name")
		if len(values) != 1 {
			return r, headerMismatch()
		}
		name, ok := decodeHeader(values[0])
		if !ok || name != r.name {
			return r, headerMismatch()
		}
		if params["requestState"] != nil || params["inputResponses"] != nil {
			return r, &failure{400, -32602, "Multi-round tool calls are outside this transport profile"}
		}
	default:
		return r, &failure{404, -32601, "Method not supported by this transport profile"}
	}
	for key := range params {
		if key == "_meta" || r.Method == "tools/call" && (key == "name" || key == "arguments") || r.Method == "tools/list" && key == "cursor" {
			continue
		}
		return r, &failure{400, -32602, "Parameter is outside this transport profile"}
	}
	if args, ok := params["arguments"]; ok {
		var object map[string]json.RawMessage
		if json.Unmarshal(args, &object) != nil || object == nil {
			return r, &failure{400, -32602, "Tool arguments must be an object"}
		}
	}
	if cursor, ok := params["cursor"]; ok {
		var value string
		if string(cursor) == "null" || json.Unmarshal(cursor, &value) != nil || len(value) > 4096 {
			return r, &failure{400, -32602, "Cursor is outside the supported bounds"}
		}
	}
	return r, nil
}

func singleHeader(h http.Header, key, want string) bool {
	values := h.Values(key)
	return len(values) == 1 && values[0] != "" && values[0] == want
}

func validID(raw json.RawMessage) bool {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	var text string
	if len(raw) != 0 && raw[0] == '"' && json.Unmarshal(raw, &text) == nil {
		return len(text) <= 128
	}
	if !json.Valid(raw) {
		return false
	}
	if len(raw) > 128 {
		return false
	}
	spelling := string(raw)
	if i := strings.IndexAny(spelling, "eE"); i >= 0 {
		exponent, err := strconv.ParseInt(spelling[i+1:], 10, 32)
		if err != nil || exponent < -64 || exponent > 64 {
			return false
		}
	}
	value, ok := new(big.Rat).SetString(string(raw))
	if !ok || !value.IsInt() || !value.Num().IsInt64() {
		return false
	}
	n := value.Num().Int64()
	return n >= -(1<<53-1) && n <= 1<<53-1
}

func decodeHeader(value string) (string, bool) {
	if strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=") {
		raw, err := base64.StdEncoding.DecodeString(value[9 : len(value)-2])
		return string(raw), err == nil && len(raw) <= 512
	}
	if value == "" || strings.TrimSpace(value) != value {
		return "", false
	}
	for _, ch := range []byte(value) {
		if ch < 0x20 || ch > 0x7e {
			return "", false
		}
	}
	return value, true
}

func accepts(h http.Header, want string) bool {
	for _, line := range h.Values("Accept") {
		for _, part := range strings.Split(line, ",") {
			kind, params, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err != nil || kind != want {
				continue
			}
			if quality, ok := params["q"]; ok {
				q, err := strconv.ParseFloat(quality, 64)
				if err != nil || math.IsNaN(q) || q <= 0 || q > 1 {
					continue
				}
			}
			return true
		}
	}
	return false
}

func sameID(a, b json.RawMessage) bool {
	if !validID(a) || !validID(b) {
		return false
	}
	var left, right any
	return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && left == right
}

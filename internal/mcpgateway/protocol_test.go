package mcpgateway

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func fixtureRequest(method string) (http.Header, []byte) {
	h := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"},
		"Mcp-Method": {method}, "Mcp-Protocol-Version": {protocolVersion}}
	params := map[string]any{"_meta": map[string]any{
		"io.modelcontextprotocol/protocolVersion":    protocolVersion,
		"io.modelcontextprotocol/clientInfo":         map[string]string{"name": "fixture", "version": "1"},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{}}}
	if method == "tools/call" {
		params["name"], params["arguments"] = "echo", map[string]any{"text": "fixture"}
		h.Set("Mcp-Name", "echo")
	}
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	return h, raw
}

func TestSupportedProtocolRequestsAndEncodedNames(t *testing.T) {
	for _, method := range []string{"server/discover", "tools/list", "tools/call"} {
		h, raw := fixtureRequest(method)
		if r, err := parseRequest(h, raw); err != nil || r.Method != method {
			t.Fatalf("supported %s refused: %+v", method, err)
		}
	}
	h, raw := fixtureRequest("tools/call")
	h.Set("Mcp-Name", "=?base64?"+base64.StdEncoding.EncodeToString([]byte("echo"))+"?=")
	if _, err := parseRequest(h, raw); err != nil {
		t.Fatalf("encoded equivalent name refused: %+v", err)
	}
}

func TestProtocolRejectsAmbiguousOrUnsupportedRequestsBeforeDispatch(t *testing.T) {
	for _, fixture := range []struct {
		name string
		edit func(http.Header, []byte) (http.Header, []byte)
		code int
	}{
		{"wrong name", func(h http.Header, b []byte) (http.Header, []byte) { h.Set("Mcp-Name", "other"); return h, b }, -32020},
		{"duplicate method header", func(h http.Header, b []byte) (http.Header, []byte) { h.Add("Mcp-Method", "tools/call"); return h, b }, -32020},
		{"missing version", func(h http.Header, b []byte) (http.Header, []byte) { h.Del("MCP-Protocol-Version"); return h, b }, -32020},
		{"disabled JSON", func(h http.Header, b []byte) (http.Header, []byte) {
			h.Set("Accept", "application/json;q=0, text/event-stream")
			return h, b
		}, -32600},
		{"nonfinite quality", func(h http.Header, b []byte) (http.Header, []byte) {
			h.Set("Accept", "application/json;q=NaN, text/event-stream")
			return h, b
		}, -32600},
		{"legacy session", func(h http.Header, b []byte) (http.Header, []byte) { h.Set("Mcp-Session-Id", "old"); return h, b }, -32600},
		{"duplicate JSON method", func(h http.Header, b []byte) (http.Header, []byte) {
			return h, []byte(strings.Replace(string(b), `"method":`, `"method":"tools/list","method":`, 1))
		}, -32600},
		{"case alias", func(h http.Header, b []byte) (http.Header, []byte) {
			return h, []byte(strings.Replace(string(b), `"method":`, `"Method":"tools/list","method":`, 1))
		}, -32600},
		{"case alias parameter", func(h http.Header, b []byte) (http.Header, []byte) {
			return h, []byte(strings.Replace(string(b), `"name":"echo"`, `"Name":"other","name":"echo"`, 1))
		}, -32602},
		{"multi-round state", func(h http.Header, b []byte) (http.Header, []byte) {
			return h, []byte(strings.Replace(string(b), `"name":"echo"`, `"requestState":"opaque","name":"echo"`, 1))
		}, -32602},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			h, raw := fixtureRequest("tools/call")
			h, raw = fixture.edit(h, raw)
			if _, err := parseRequest(h, raw); err == nil || err.code != fixture.code {
				t.Fatalf("bad fixture passed or returned wrong code: %+v", err)
			}
		})
	}
	for _, method := range []string{"initialize", "ping"} {
		h, raw := fixtureRequest(method)
		if _, err := parseRequest(h, raw); err == nil || err.code != -32601 {
			t.Fatal("removed method was advertised as supported")
		}
	}
}

func TestStrictJSONBoundsAndEquivalentResponseIDs(t *testing.T) {
	for _, raw := range []string{`{"x":1,"x":2}`, `{"x":{"a":1,"\u0061":2}}`, `{} {}`, strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34), string([]byte{'"', 0xff, '"'})} {
		if checkJSON([]byte(raw)) == nil {
			t.Fatal("ambiguous, deep or malformed JSON accepted")
		}
	}
	if !sameID(json.RawMessage(`"\u0061"`), json.RawMessage(`"a"`)) || sameID(json.RawMessage(`1`), json.RawMessage(`"1"`)) || sameID(json.RawMessage(`null`), json.RawMessage(`null`)) {
		t.Fatal("response ID matching lost JSON semantics or accepted a notification")
	}
}

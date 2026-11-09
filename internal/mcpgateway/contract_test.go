package mcpgateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestMirroredHeaderDefinitionsExcludeOnlyInvalidTools(t *testing.T) {
	for _, schema := range []string{
		`{"properties":{"region":{"type":"string","x-mcp-header":"Region"}}}`,
		`{"properties":{"user":{"properties":{"id":{"type":"integer","x-mcp-header":"ID"}}}}}`,
	} {
		if !validHeaderSchema([]byte(schema)) {
			t.Fatal("valid static header path excluded")
		}
	}
	for _, schema := range []string{
		`{"type":"string","x-mcp-header":"Root"}`,
		`{"properties":{"region":{"type":"string","x-mcp-header":""}}}`,
		`{"properties":{"region":{"type":"string","x-mcp-header":"bad name"}}}`,
		`{"properties":{"region":{"type":"number","x-mcp-header":"Region"}}}`,
		`{"properties":{"a":{"type":"string","x-mcp-header":"Region"},"b":{"type":"boolean","x-mcp-header":"region"}}}`,
		`{"properties":{"region":{"items":{"type":"string","x-mcp-header":"Region"}}}}`,
		`{"allOf":[{"properties":{"region":{"type":"string","x-mcp-header":"Region"}}}]}`,
		`{"$defs":{"region":{"type":"string","x-mcp-header":"Region"}}}`,
	} {
		if validHeaderSchema([]byte(schema)) {
			t.Fatal("invalid header definition accepted")
		}
		raw := `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","tools":[{"name":"bad","inputSchema":` + schema + `},{"name":"echo","inputSchema":{}}]}}`
		got, final, err := response([]byte(raw), request{ID: json.RawMessage(`1`), Method: "tools/list"}, map[string]bool{"bad": true, "echo": true}, 1<<20, "")
		if err != nil || !final || strings.Contains(string(got), `"name":"bad"`) || !strings.Contains(string(got), `"name":"echo"`) {
			t.Fatal("invalid tool hid a valid sibling")
		}
	}
}

func TestNotificationsMustMatchExplicitRequestProgress(t *testing.T) {
	expected := request{ID: json.RawMessage(`1`), Method: "tools/call", progress: json.RawMessage(`"job"`)}
	good := `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"job","progress":1,"total":2}}`
	if _, final, err := response([]byte(good), expected, nil, 4096, ""); err != nil || final {
		t.Fatal("requested progress refused")
	}
	for _, raw := range []string{
		strings.Replace(good, `"job"`, `"other"`, 1), strings.Replace(good, `"progress":1`, `"progress":null`, 1),
		strings.Replace(good, `"progress":1`, `"progress":-1`, 1), strings.Replace(good, `"progress":1`, `"Progress":1`, 1),
		`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"error","data":"unexpected"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","content":[]},"params":{}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"Code":-1,"message":"ambiguous"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","content":[],"task":{}}}`,
	} {
		if _, _, err := response([]byte(raw), expected, nil, 4096, ""); err == nil {
			t.Fatal("unsolicited or ambiguous response accepted")
		}
	}
	expected.progress = nil
	if _, _, err := response([]byte(good), expected, nil, 4096, ""); err == nil {
		t.Fatal("unrequested progress forwarded")
	}
}

func TestRequestIDsUseBoundedJSONValueSemantics(t *testing.T) {
	for _, id := range []string{`""`, `1.0`, `1e0`, `-9007199254740991`, `9007199254740991`} {
		if !validID(json.RawMessage(id)) {
			t.Fatal("supported ID refused")
		}
	}
	for _, id := range []string{`null`, `true`, `1.000000000000000001`, `9007199254740992`, `1/1`, `1e1000000000`, `1e-1000000000`} {
		if validID(json.RawMessage(id)) {
			t.Fatal("invalid or inexact ID accepted")
		}
	}
	if !sameID(json.RawMessage(`1e0`), json.RawMessage(`1.0`)) {
		t.Fatal("equivalent exact IDs differ")
	}
}

func TestUnknownMCPHeadersStayOpaqueAndCannotAuthorizeTools(t *testing.T) {
	h, raw := fixtureRequest("tools/call")
	h.Add("Mcp-Param-Region", "=?base64?cHJpdmF0ZQ==?=")
	if _, problem := parseRequest(h, raw); problem != nil {
		t.Fatal("opaque intermediary header refused")
	}
	h.Set("Last-Event-ID", "resume")
	if _, problem := parseRequest(h, raw); problem == nil {
		t.Fatal("resumption accepted")
	}
	for _, method := range []string{"tasks/get", "resources/read", "subscriptions/listen"} {
		h, raw = fixtureRequest(method)
		if _, problem := parseRequest(h, raw); problem == nil || problem.status != http.StatusNotFound {
			t.Fatal("unsupported method accepted")
		}
	}
}

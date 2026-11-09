package mcpgateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRepliesMatchTheRequestAndRejectUnsupportedServerMessages(t *testing.T) {
	expected := request{ID: json.RawMessage(`1`), Method: "tools/call"}
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":2,"result":{"resultType":"complete"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":1,"message":"bad"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","requestState":"state"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"sampling/createMessage","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete"},"Method":"sampling/createMessage"}`,
		`{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","ResultType":"input_required"}}`,
		`{"jsonrpc":"2.0","id":1,"error":null}`,
	} {
		if _, _, err := response([]byte(raw), expected, nil, 1<<20, ""); err == nil {
			t.Fatal("unqualified reply passed")
		}
	}
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","content":[{"type":"text","text":"ok"}]}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid input"}}`,
	} {
		if _, final, err := response([]byte(raw), expected, nil, 1<<20, ""); err != nil || !final {
			t.Fatalf("valid final reply refused: %v", err)
		}
	}
}

func TestToolDiscoveryFiltersAuthorityAndDisablesPublicCaching(t *testing.T) {
	expected := request{ID: json.RawMessage(`1`), Method: "tools/list"}
	raw := `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","cacheScope":"public","ttlMs":60000,"tools":[` +
		`{"name":"private","inputSchema":{}},{"name":"echo","inputSchema":{"type":"object"}},{"name":"alpha","inputSchema":{}}]}}`
	got, final, err := response([]byte(raw), expected, map[string]bool{"echo": true, "alpha": true}, 1<<20, "")
	if err != nil || !final || strings.Contains(string(got), `"name":"private"`) || !strings.Contains(string(got), `"cacheScope":"private"`) || !strings.Contains(string(got), `"ttlMs":0`) {
		t.Fatalf("authority or cache filtering failed: %v", err)
	}
	if strings.Index(string(got), `"name":"alpha"`) > strings.Index(string(got), `"name":"echo"`) {
		t.Fatal("tool list ordering changed between equivalent discovery calls")
	}
	for _, tools := range []string{
		`[{"name":"echo","Name":"private","inputSchema":{}}]`,
		`[{"name":"echo","inputSchema":null}]`,
		`[{"name":"echo","inputSchema":{}},{"name":"echo","inputSchema":{}}]`,
	} {
		raw := `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","tools":` + tools + `}}`
		if _, _, err := response([]byte(raw), expected, map[string]bool{"echo": true}, 1<<20, ""); err == nil {
			t.Fatal("ambiguous tool contract passed")
		}
	}
}

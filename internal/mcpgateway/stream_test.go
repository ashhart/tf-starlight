package mcpgateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBoundedStreamForwardsOneFinalResponse(t *testing.T) {
	final := `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","content":[]}}`
	body := ": ping\n\ndata: " + final + "\n\ndata: " + final + "\n\n"
	w := httptest.NewRecorder()
	if err := relayEvents(context.Background(), w, strings.NewReader(body), request{ID: json.RawMessage(`1`), Method: "tools/call"}, nil, 1<<20, 64<<10); err != nil {
		t.Fatal(err)
	}
	if strings.Count(w.Body.String(), `"resultType":"complete"`) != 1 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("stream repeated its final response or lost private cache policy")
	}
}

func TestStreamLimitsAndTruncatedOrUnsolicitedRepliesFailClosed(t *testing.T) {
	for _, body := range []string{
		"data: {\"jsonrpc\":\"2.0\",\"id\":1}",
		"data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"resultType\":\"complete\"}}\n\n",
		"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"roots/list\"}\n\n",
		": " + strings.Repeat("x", 2048) + "\n\n",
	} {
		w := httptest.NewRecorder()
		if err := relayEvents(context.Background(), w, strings.NewReader(body), request{ID: json.RawMessage(`1`), Method: "tools/call"}, nil, 4096, 1024); err == nil {
			t.Fatal("unqualified stream passed")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	err := relayEvents(ctx, w, strings.NewReader(": keepalive\n\n"), request{ID: json.RawMessage(`1`)}, nil, 4096, 1024)
	if err == nil || w.Body.Len() != 0 {
		t.Fatal("cancelled stream wrote another message")
	}
}

func TestProgressStreamCannotRepeatOrRegressItsCounter(t *testing.T) {
	progress := `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"job","progress":1}}`
	final := `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","content":[]}}`
	for _, second := range []string{progress, strings.Replace(progress, `"progress":1`, `"progress":0`, 1)} {
		body := "data: " + progress + "\n\ndata: " + second + "\n\ndata: " + final + "\n\n"
		w := httptest.NewRecorder()
		err := relayEvents(context.Background(), w, strings.NewReader(body), request{ID: json.RawMessage(`1`), Method: "tools/call", progress: json.RawMessage(`"job"`)}, nil, 4096, 1024)
		if err == nil || strings.Count(w.Body.String(), "notifications/progress") != 1 || strings.Contains(w.Body.String(), "resultType") {
			t.Fatal("invalid progress stream completed")
		}
	}
}

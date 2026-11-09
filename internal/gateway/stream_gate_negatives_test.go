package gateway

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ashhart/tf-starlight/internal/providers"
)

type gateFailedFlush struct{ http.ResponseWriter }

func (w gateFailedFlush) FlushError() error {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
	return errors.New("fixture flush failure")
}

func TestStreamGateRejectsAToolRequestWithOnlyText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	if _, err := runStreamGate(server.URL, "fixture", "", streamGateCases[1]); err == nil {
		t.Fatal("tool fixture without a tool payload passed")
	}
}

func TestStreamGateRejectsNonLoopbackAndCredentialedEndpointsBeforeContact(t *testing.T) {
	var contacted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { contacted.Store(true) }))
	defer server.Close()
	credentialed := strings.Replace(server.URL, "http://", "http://fixture:fixture@", 1)
	for _, endpoint := range []string{"http://192.0.2.1", "https://example.invalid", credentialed, server.URL + "?credential=fixture"} {
		if _, err := runStreamGate(endpoint, "fixture", "", streamGateCases[0]); err == nil {
			t.Fatal("unsafe gate endpoint accepted")
		}
	}
	if contacted.Load() {
		t.Fatal("invalid endpoint was contacted")
	}
}

func TestStreamGateRejectsAFlushErrorAfterTheBodyWasDelivered(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	wrap := func(w http.ResponseWriter) http.ResponseWriter { return gateFailedFlush{w} }
	if _, err := runStreamGate(server.URL, "fixture", "", streamGateCases[0], wrap); err == nil {
		t.Fatal("flush error passed qualification")
	}
}

func TestIndependentClientFixtureDecoderIgnoresMetadata(t *testing.T) {
	for _, body := range []string{
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"id":"call_fixture"}]}}]}`,
		`{"choices":[],"usage":{"completion_tokens":1}}`,
		`{"error":{"message":"fixture failure"}}`,
	} {
		if kind, tool := clientGatePayload(providers.ChatCompletions, []byte(body)); kind != "" || tool {
			t.Fatal("metadata became client output")
		}
	}
	for _, c := range []struct {
		protocol, body, kind string
		tool                 bool
	}{
		{providers.ChatCompletions, `{"choices":[{"delta":{"content":"hi","reasoning_content":"why"}}]}`, "text", false},
		{providers.ChatCompletions, `{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{"}}]}}]}`, "tool", true},
		{providers.Responses, `{"type":"response.reasoning_text.delta","delta":"why"}`, "reasoning", false},
		{providers.Messages, `{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{"}}`, "tool", true},
	} {
		kind, tool := clientGatePayload(c.protocol, []byte(c.body))
		if kind != c.kind || tool != c.tool {
			t.Fatalf("independent client payload %q/%v, want %q/%v", kind, tool, c.kind, c.tool)
		}
	}
}

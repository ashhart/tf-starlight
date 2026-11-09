package mcpgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ashhart/tf-starlight/internal/identity/principal"
)

type readSpy struct{ reads int }

func (s *readSpy) Read([]byte) (int, error) { s.reads++; return 0, io.EOF }
func (s *readSpy) Close() error             { return nil }

func TestDeniedAuthorityCannotReadTheBodyOrHoldToolCapacity(t *testing.T) {
	for _, full := range []bool{false, true} {
		h, calls, events := handlerFixture(t, http.HandlerFunc(complete))
		h.options.Authorize = func(_ context.Context, _ *http.Request, raw []byte) (*principal.Principal, int) {
			if raw != nil || len(h.slots) != 0 && !full {
				t.Fatal("authorization happened after body/capacity")
			}
			return nil, 401
		}
		if full {
			h.slots <- struct{}{}
		}
		headers, _ := fixtureRequest("tools/call")
		r := httptest.NewRequest("POST", "https://ai.example.com/mcp", nil)
		r.Header = headers
		spy := &readSpy{}
		r.Body = spy
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 || spy.reads != 0 || calls.Load() != 0 || len(*events) != 0 {
			t.Fatal("unauthenticated request consumed work or wrote an individual denial")
		}
		h.Close()
		if len(*events) != 1 || (*events)[0].Phase != "refusals" || (*events)[0].RefusalCounts["401"] != 1 || (*events)[0].AttemptID != "" || (*events)[0].Actor != nil {
			t.Fatal("anonymous refusal summary lost its count or claimed an identity")
		}
	}
}

func TestAttemptPairingDoesNotDependOnMiddlewareOrClientRPCIDs(t *testing.T) {
	h, _, events := handlerFixture(t, http.HandlerFunc(complete))
	a := callHandler(h, "tools/call", nil)
	b := callHandler(h, "tools/call", nil)
	if len(*events) != 4 {
		t.Fatal("attempt events lost")
	}
	first, second := a.Header().Get("X-Starlight-MCP-Attempt-ID"), b.Header().Get("X-Starlight-MCP-Attempt-ID")
	if first == "" || second == "" || first == second || (*events)[0].AttemptID != first || (*events)[1].AttemptID != first || (*events)[2].AttemptID != second || (*events)[3].AttemptID != second {
		t.Fatal("attempt pairing reused a client id or required middleware")
	}
}

func TestSSECompactsMultilineDataAndBoundsEncodedOutputAndFrames(t *testing.T) {
	input := "data: {\"jsonrpc\":\"2.0\",\n" + "data: \"id\":1,\"result\":{\"resultType\":\"complete\",\"content\":[]}}\n\n"
	w := httptest.NewRecorder()
	if _, err := relayEvents(context.Background(), w, strings.NewReader(input), request{ID: json.RawMessage(`1`), Method: "tools/call"}, nil, 4096, 1024, ""); err != nil {
		t.Fatal(err)
	}
	if bytes.Count(w.Body.Bytes(), []byte("data:")) != 1 || !json.Valid(rpcEvent(w.Body.Bytes())) {
		t.Fatal("multiline JSON broke SSE framing")
	}
	for _, fixture := range []struct {
		input     string
		limit     int64
		maxOutput int
	}{
		{strings.Repeat("\n", 8), 25, 25},
		{strings.Repeat("\n", 4097), 128 << 10, 4096 * len(": keepalive\n\n")},
	} {
		w = httptest.NewRecorder()
		if _, err := relayEvents(context.Background(), w, strings.NewReader(fixture.input), request{}, nil, fixture.limit, 1024, ""); err == nil || w.Body.Len() > fixture.maxOutput {
			t.Fatal("keepalives escaped output/frame bounds")
		}
	}
}

func TestUpstreamCredentialCannotBeReflectedThroughJSONEscapes(t *testing.T) {
	for _, secret := range []string{"fixture-secret", `fixture-\u0073ecret`} {
		raw := []byte(`{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","content":[{"type":"text","text":"` + secret + `"}]}}`)
		if _, _, err := response(raw, request{ID: json.RawMessage(`1`), Method: "tools/call"}, nil, 4096, "fixture-secret"); err == nil {
			t.Fatal("upstream credential reflected")
		}
	}
	raw := []byte(`{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","content":[],"structuredContent":{"opaque_number":1e9999}}}`)
	if _, _, err := response(raw, request{ID: json.RawMessage(`1`), Method: "tools/call"}, nil, 4096, "fixture-secret"); err != nil {
		t.Fatal("credential inspection coerced an opaque JSON number")
	}
}

func TestReservedMetadataAliasesAreRefused(t *testing.T) {
	h, raw := fixtureRequest("tools/call")
	for _, key := range []string{"io.modelcontextprotocol/protocolVersion", "io.modelcontextprotocol/clientInfo", "io.modelcontextprotocol/clientCapabilities"} {
		bad := strings.Replace(string(raw), `"`+key+`":`, `"`+strings.ToUpper(key)+`":null,"`+key+`":`, 1)
		if _, problem := parseRequest(h, []byte(bad)); problem == nil {
			t.Fatal("reserved metadata alias accepted")
		}
	}
}

func TestAnonymousRefusalsDoNotWritePerRequest(t *testing.T) {
	h, calls, _ := handlerFixture(t, http.HandlerFunc(complete))
	h.options.Authorize = func(context.Context, *http.Request, []byte) (*principal.Principal, int) { return nil, 401 }
	var writes atomic.Int32
	h.options.Record = func(context.Context, Event) error { writes.Add(1); return nil }
	for range 100 {
		if w := callHandler(h, "tools/call", nil); w.Code != 401 {
			t.Fatal("anonymous refusal status changed")
		}
	}
	if writes.Load() != 0 || calls.Load() != 0 {
		t.Fatalf("anonymous flood caused %d synchronous audit writes", writes.Load())
	}
}

func TestAuthenticatedDenialSurvivesCancelledRequest(t *testing.T) {
	h, calls, _ := handlerFixture(t, http.HandlerFunc(complete))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var recorded bool
	h.options.Authorize = func(context.Context, *http.Request, []byte) (*principal.Principal, int) {
		return &principal.Principal{Type: principal.User, ID: "fixture"}, 403
	}
	h.options.Record = func(ctx context.Context, e Event) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Actor == nil || e.Phase != "deny" || e.AttemptID == "" {
			return errors.New("missing authenticated denial")
		}
		recorded = true
		return nil
	}
	w := callHandler(h, "tools/call", func(r *http.Request) { *r = *r.WithContext(ctx) })
	if w.Code != 403 || !recorded || calls.Load() != 0 {
		t.Fatal("cancelled request lost its authenticated denial")
	}
}

func TestAdditionalReservedMetadataAliasesAreRefused(t *testing.T) {
	h, raw := fixtureRequest("tools/call")
	for _, key := range []string{"io.modelcontextprotocol/logLevel", "progressToken", "io.modelcontextprotocol/related-task"} {
		bad := strings.Replace(string(raw), `"_meta":{`, `"_meta":{"`+strings.ToUpper(key)+`":null,`, 1)
		if bad == string(raw) {
			t.Fatal("alias fixture did not modify metadata")
		}
		if _, problem := parseRequest(h, []byte(bad)); problem == nil {
			t.Fatalf("reserved alias %s accepted", key)
		}
	}
}

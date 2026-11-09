package mcpgateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/config"
	"github.com/ashhart/tf-starlight/internal/identity/principal"
	"github.com/ashhart/tf-starlight/internal/ids"
)

func handlerFixture(t *testing.T, upstream http.Handler) (*Handler, *atomic.Int32, *[]Event) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		upstream.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	var mu sync.Mutex
	events := []Event{}
	h, err := New(Options{PublicURL: "https://ai.example.com", Config: config.MCP{Enabled: true, Endpoint: server.URL,
		Internal: true, ProjectID: ids.New(), KeyIDs: []string{ids.New()}, Tools: []string{"echo"}, MaxClass: "internal",
		MaxConcurrent: 1, Timeout: config.Duration{Duration: 2 * time.Second}}, Offline: func() bool { return false },
		Authorize: func(context.Context, *http.Request, []byte) (*principal.Principal, int) {
			return &principal.Principal{Type: principal.User, ID: "fixture"}, 0
		}, Record: func(_ context.Context, e Event) error {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, e)
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, &calls, &events
}

func callHandler(h *Handler, method string, edit func(*http.Request)) *httptest.ResponseRecorder {
	headers, raw := fixtureRequest(method)
	r := httptest.NewRequest("POST", "https://ai.example.com/mcp", bytes.NewReader(raw))
	r.Header = headers
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func complete(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","content":[]}}`)
}

func TestHandlerPolicyRejectionsCannotDispatch(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		edit   func(*Handler, *http.Request)
		status int
	}{
		{"origin", func(_ *Handler, r *http.Request) { r.Header.Set("Origin", "https://other.example.com") }, 403},
		{"method", func(_ *Handler, r *http.Request) { r.Method = "GET" }, 405},
		{"authority", func(h *Handler, _ *http.Request) {
			h.options.Authorize = func(context.Context, *http.Request, []byte) (*principal.Principal, int) { return nil, 403 }
		}, 403},
		{"accounting", func(h *Handler, _ *http.Request) {
			h.options.Record = func(context.Context, Event) error { return errors.New("fixture") }
		}, 503},
		{"tool", func(h *Handler, _ *http.Request) { h.allowed = map[string]bool{} }, 403},
		{"capacity", func(h *Handler, _ *http.Request) { h.slots <- struct{}{} }, 429},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			h, calls, _ := handlerFixture(t, http.HandlerFunc(complete))
			w := callHandler(h, "tools/call", func(r *http.Request) { fixture.edit(h, r) })
			if w.Code != fixture.status || calls.Load() != 0 {
				t.Fatalf("status=%d calls=%d", w.Code, calls.Load())
			}
		})
	}
}

func TestHandlerAccountsOneExchangeAndServesOnlyLocalDiscovery(t *testing.T) {
	h, calls, events := handlerFixture(t, http.HandlerFunc(complete))
	w := callHandler(h, "tools/call", nil)
	if w.Code != 200 || calls.Load() != 1 || len(*events) != 2 || (*events)[0].Phase != "begin" ||
		(*events)[1].Phase != "end" || !(*events)[1].DispatchPossible || (*events)[1].Outcome != "response_complete" {
		t.Fatal("exchange accounting lost")
	}
	w = callHandler(h, "server/discover", nil)
	if w.Code != 200 || calls.Load() != 1 || !bytes.Contains(w.Body.Bytes(), []byte(`"tools":{}`)) || bytes.Contains(w.Body.Bytes(), []byte(`"resources"`)) {
		t.Fatal("discovery advertised unsupported authority")
	}
}

func TestHandlerStreamFailureNeverWritesASecondHTTPEnvelope(t *testing.T) {
	h, calls, events := handlerFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": keepalive\n\ndata: malformed\n\n")
	}))
	w := callHandler(h, "tools/call", nil)
	if w.Code != 200 || w.Body.String() != ": keepalive\n\n" || calls.Load() != 1 || (*events)[1].Outcome != "upstream_incomplete" {
		t.Fatal("partial stream was converted to a false final response")
	}
}

func TestHandlerDisconnectCancelsItsSingleUpstreamCall(t *testing.T) {
	cancelled := make(chan struct{})
	h, calls, _ := handlerFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": keepalive\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	listener := httptest.NewServer(h)
	defer listener.Close()
	headers, raw := fixtureRequest("tools/call")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", listener.URL, bytes.NewReader(raw))
	r.Header = headers
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	res.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream survived client disconnect")
	}
	if calls.Load() != 1 {
		t.Fatal("cancelled call retried")
	}
}

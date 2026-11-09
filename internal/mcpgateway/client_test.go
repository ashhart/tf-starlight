package mcpgateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpstreamUsesOnlyItsOwnCredentialAndValidatedMCPHeaders(t *testing.T) {
	var called atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		if r.Header.Get("Authorization") != "Bearer upstream-fixture" || r.Header.Get("Cookie") != "" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("Mcp-Name") != "echo" || r.Header.Get("Mcp-Param-Zone") != "opaque-zone" {
			t.Error("credential boundary or MCP routing header changed")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","content":[]}}`)
	}))
	defer server.Close()
	u, err := newUpstream(server.URL, "upstream-fixture", true, func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	defer u.HTTP.CloseIdleConnections()
	h, raw := fixtureRequest("tools/call")
	h.Set("Authorization", "Bearer caller-fixture")
	h.Set("Cookie", "caller-cookie")
	h.Set("Idempotency-Key", "caller-retry-key")
	h.Set("Mcp-Param-Zone", "opaque-zone")
	x, err := u.send(context.Background(), h, raw)
	if err != nil || !x.dispatched || called.Load() != 1 {
		t.Fatalf("qualified single exchange failed: %v", err)
	}
	x.close()
}

func TestUpstreamOfflineAndInvalidProtocolCannotDispatch(t *testing.T) {
	var called atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Add(1) }))
	defer server.Close()
	for _, offline := range []bool{true, false} {
		u, err := newUpstream(server.URL, "", true, func() bool { return offline })
		if err != nil {
			t.Fatal(err)
		}
		h, raw := fixtureRequest("tools/call")
		if !offline {
			h.Set("Mcp-Name", "other")
		}
		if x, err := u.send(context.Background(), h, raw); err == nil || x.dispatched {
			t.Fatal("offline or invalid call dispatched")
		}
		u.HTTP.CloseIdleConnections()
	}
	if called.Load() != 0 {
		t.Fatal("unqualified call reached upstream")
	}
}

func TestUpstreamDoesNotRetryAfterTheRequestWasReceived(t *testing.T) {
	var called atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		connection.Close()
	}))
	defer server.Close()
	u, err := newUpstream(server.URL, "", true, func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	defer u.HTTP.CloseIdleConnections()
	h, raw := fixtureRequest("tools/call")
	x, err := u.send(context.Background(), h, raw)
	if err == nil || !x.dispatched || called.Load() != 1 {
		t.Fatalf("ambiguous execution was retried or lost: count=%d, err=%v", called.Load(), err)
	}
}

func TestClosingTheExchangeCancelsTheUpstreamRequest(t *testing.T) {
	cancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer server.Close()
	u, err := newUpstream(server.URL, "", true, func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	defer u.HTTP.CloseIdleConnections()
	h, raw := fixtureRequest("tools/call")
	x, err := u.send(context.Background(), h, raw)
	if err != nil {
		t.Fatal(err)
	}
	x.close()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream survived a closed exchange")
	}
}

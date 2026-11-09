package gateway

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/httpx"
	"github.com/ashhart/tf-starlight/internal/providers"
)

func TestHTTPRelayObservesModelOutputAfterHeadersAndHeartbeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release := make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	generated := make(chan time.Time, 1)
	payload := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\"}}]}}]}\n\n"
	usage := "data: {\"choices\":[],\"usage\":{\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, ": heartbeat\n\n")
		_ = http.NewResponseController(w).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		generated <- time.Now()
		_, _ = io.WriteString(w, payload+usage)
	}))
	defer upstream.Close()
	type result struct {
		start time.Time
		out   relayed
	}
	results := make(chan result, 1)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream.URL, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp, err := upstream.Client().Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		out := relay(r.Context(), w, resp, providers.ChatCompletions, false, httpx.OpenAI, start, true)
		results <- result{start: start, out: out}
	}))
	defer gateway.Close()
	defer unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gateway.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := gateway.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	line, err := r.ReadString('\n')
	if err != nil || line != ": heartbeat\n" {
		t.Fatalf("first client line %q: %v", line, err)
	}
	if _, err := r.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	heartbeatAt := time.Now()
	unlock()
	rest, err := io.ReadAll(r)
	if err != nil || string(rest) != payload+usage {
		t.Fatalf("forwarded model bytes %q: %v", rest, err)
	}
	var got result
	select {
	case got = <-results:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	modelAt := <-generated
	if got.out.outcome != "ok" || got.out.times.kind != toolPayload {
		t.Fatalf("relay result %+v", got.out)
	}
	if got.start.Add(got.out.firstByte).After(heartbeatAt) || got.start.Add(got.out.times.data.elapsed).After(heartbeatAt) {
		t.Fatal("headers and initial transport bytes should precede the client's heartbeat receipt")
	}
	if got.start.Add(got.out.times.output.elapsed).Before(modelAt) || !got.out.times.flush.seen {
		t.Fatalf("model output was timed before it was generated: %+v", got.out.times)
	}
	if strings.Contains(string(rest), "heartbeat") {
		t.Fatal("client stream was duplicated")
	}
}

package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/node/nodetest"
	"github.com/ashhart/tf-starlight/internal/telemetry"
	"github.com/ashhart/tf-starlight/internal/testutil/tracecollector"
)

type delayedAcquire struct{ Target }

func (d delayedAcquire) Acquire() (func(), error) {
	time.Sleep(30 * time.Millisecond)
	return d.Target.Acquire()
}

type phaseTransport struct {
	parent string
}

func (p *phaseTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p.parent = r.Header.Get("Traceparent")
	time.Sleep(40 * time.Millisecond)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
		Body: &phaseBody{Reader: strings.NewReader("data: [DONE]\n\n")}}, nil
}

type phaseBody struct {
	*strings.Reader
	started bool
}

func (b *phaseBody) Read(p []byte) (int, error) {
	if !b.started {
		time.Sleep(60 * time.Millisecond)
		b.started = true
	}
	return b.Reader.Read(p)
}

func (b *phaseBody) Close() error { return nil }

func TestNodePhasesFollowGatewayContextAndAttributeControlledDelays(t *testing.T) {
	f := newFixture(t, 0)
	collector := tracecollector.New(t)
	r, err := telemetry.New(context.Background(), telemetry.Options{Endpoint: collector.URL(), Component: telemetry.NodeComponent,
		SamplePerMillion: 1000000, Queue: 32, Batch: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	transport := &phaseTransport{}
	f.srv.Telemetry, f.srv.Transport = r, transport
	f.srv.Backend = backend{f.replica: delayedAcquire{f.tgt}}
	body := strings.Replace(chatBody, "hi there", "private-prompt-canary", 1)
	tok := nodetest.Permit(t, f.signer, f.claims("/v1/chat/completions"), []byte(body))
	req := f.request(t, "POST", "/v1/chat/completions", body, tok)
	id, parent := "0123456789abcdef0123456789abcdef", "1234567890abcdef"
	req.Header.Set("Traceparent", "00-"+id+"-"+parent+"-01")
	req.Header.Set("Tracestate", "vendor=private-vendor-canary")
	req.Header.Set("Baggage", "private-caller-canary")
	w := httptest.NewRecorder()
	f.srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "data: [DONE]\n\n" {
		t.Fatalf("instrumentation changed relay bytes: %d %q", w.Code, w.Body.String())
	}
	frames := collector.Wait(t, id, "node.request", "node.permit", "node.engine_request", "node.engine_headers", "node.engine_relay")
	if frames["node.request"].Parent != parent || frames["node.engine_request"].Parent != frames["node.request"].ID ||
		transport.parent != "00-"+id+"-"+frames["node.engine_request"].ID+"-01" {
		t.Fatal("gateway/node/engine HTTP context was not connected")
	}
	for name, minimum := range map[string]time.Duration{"node.permit": 30 * time.Millisecond,
		"node.engine_headers": 40 * time.Millisecond, "node.engine_relay": 60 * time.Millisecond} {
		f := frames[name]
		if duration := time.Duration(f.End - f.Start); duration < minimum {
			t.Fatalf("controlled delay attributed to the wrong phase: %s=%v", name, duration)
		}
	}
	collector.AssertPrivate(t, "private-prompt-canary", "private-vendor-canary", "private-caller-canary", "engine-key", tok)
	io.Copy(io.Discard, w.Result().Body)
}

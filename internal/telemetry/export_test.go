package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOTLPEncodingAndPartialRejectionDoNotExportErrorPayloads(t *testing.T) {
	var requests atomic.Int64
	payload := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		b, _ := io.ReadAll(req.Body)
		payload <- b
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"partialSuccess":{"rejectedSpans":"1","errorMessage":"private collector message"}}`)
	}))
	defer srv.Close()
	o := options(srv.URL)
	o.Batch = 2
	r, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, root := r.Root(context.Background(), "req_0123456789abcdef0123456789abcdef")
	_, child := r.Start(ctx, GatewayPrepare)
	child.End(OK)
	root.End(Unset)
	eventually(t, func() bool { return r.Stats().Partial == 1 })
	if s := r.Stats(); requests.Load() != 1 || s.Sent != 1 || s.Rejected != 1 || s.Retries != 0 {
		t.Fatalf("partial success was retried or lost: %+v", s)
	}
	b := <-payload
	var wire map[string]json.RawMessage
	if json.Unmarshal(b, &wire) != nil || len(wire) != 1 || wire["resourceSpans"] == nil {
		t.Fatalf("not an OTLP request: %s", b)
	}
	for _, forbidden := range []string{"private", "prompt", "credentials", "request_id", "model", "replica", "attributes\":null"} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("unexpected trace payload field: %s", forbidden)
		}
	}
	if !strings.Contains(string(b), `"traceId":"0123456789abcdef0123456789abcdef"`) ||
		!strings.Contains(string(b), `"startTimeUnixNano":"`) || !strings.Contains(string(b), `"kind":2`) {
		t.Fatal("OTLP IDs, integer enums or decimal-string timestamps were encoded incorrectly")
	}
}

func TestExporterRetriesOnceAndRejectsOversizedResponses(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		name := "retry"
		if oversized {
			name = "oversized"
		}
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				n := requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if oversized {
					io.WriteString(w, strings.Repeat("x", responseLimit+1))
				} else if n == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
				} else {
					io.WriteString(w, "{}")
				}
			}))
			defer srv.Close()
			r, err := New(context.Background(), options(srv.URL))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			_, span := r.Root(context.Background(), "req_0123456789abcdef0123456789abcdef")
			span.End(OK)
			eventually(t, func() bool { return r.Stats().Sent+r.Stats().Failed == 1 })
			s := r.Stats()
			if oversized && (requests.Load() != 1 || s.Failed != 1) || !oversized && (requests.Load() != 2 || s.Retries != 1 || s.Sent != 1) {
				t.Fatalf("retry/response bounds: %+v requests=%d", s, requests.Load())
			}
		})
	}
}

func TestAllRejectedSpansDoNotRefreshAcceptanceHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"partialSuccess":{"rejectedSpans":"1"}}`)
	}))
	defer srv.Close()
	r, err := New(context.Background(), options(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, span := r.Root(context.Background(), "req_0123456789abcdef0123456789abcdef")
	span.End(OK)
	eventually(t, func() bool { return r.Stats().Rejected == 1 })
	if s := r.Stats(); s.Sent != 0 || s.LastSuccess != 0 || s.Retries != 0 {
		t.Fatalf("rejection presented as acceptance: %+v", s)
	}
}

package telemetry

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func eventually(t *testing.T, ready func() bool) {
	t.Helper()
	until := time.NewTimer(4 * time.Second)
	defer until.Stop()
	for !ready() {
		select {
		case <-until.C:
			t.Fatal("timed out waiting for telemetry fixture")
		case <-time.After(time.Millisecond):
		}
	}
}

func options(endpoint string) Options {
	return Options{Endpoint: endpoint, SamplePerMillion: 1_000_000, Queue: 4, Batch: 1,
		Timeout: time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestSlowCollectorCannotHoldInferenceOrGrowItsQueue(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.Copy(io.Discard, req.Body)
		close(entered)
		select {
		case <-release:
		case <-req.Context().Done():
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{}")
	}))
	defer srv.Close()
	defer close(release)
	o := options(srv.URL)
	o.Timeout = 5 * time.Second
	r, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, root := r.Root(context.Background(), "req_0123456789abcdef0123456789abcdef")
	root.End(OK)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("collector was never entered")
	}
	done := make(chan struct{})
	go func() {
		for range 1000 {
			_, span := r.Start(ctx, GatewayRelay)
			span.End(Unset)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("inference-side recording waited for the collector")
	}
	s := r.Stats()
	if s.QueueLength > 4 || s.QueueFull < 996 || s.Sent != 0 {
		t.Fatalf("unbounded or hidden saturation: %+v", s)
	}
	r.Close()
	if s := r.Stats(); s.QueueLength != 0 || s.Shutdown == 0 {
		t.Fatalf("shutdown did not account for its bounded pending work: %+v", s)
	}
	select {
	case <-r.done:
	case <-time.After(time.Second):
		t.Fatal("collector cancellation left the export worker running")
	}
}

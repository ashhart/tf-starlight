package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestDisabledRecorderAndConfigurationBounds(t *testing.T) {
	r, err := New(context.Background(), Options{})
	if err != nil || r != nil {
		t.Fatalf("default unexpectedly starts telemetry: %v, %v", r, err)
	}
	for _, change := range []func(*Options){
		func(o *Options) { o.Queue = 4097 }, func(o *Options) { o.Batch = 129 },
		func(o *Options) { o.Timeout = time.Hour }, func(o *Options) { o.SamplePerMillion = 1000001 },
		func(o *Options) { o.Endpoint = "https://user:fixture@collector.example/v1/traces" },
		func(o *Options) { o.Endpoint = "https://collector.example/v1/traces?key=fixture" },
		func(o *Options) { o.Endpoint = "http://collector.example/v1/traces"; o.BearerTokenFile = "fixture" },
	} {
		o := options("https://collector.example/v1/traces")
		change(&o)
		if o.Validate() == nil {
			t.Fatal("invalid or unbounded exporter configuration accepted")
		}
	}
}

func TestCollectorRedirectCannotForwardBearerOrTracePayload(t *testing.T) {
	var reached atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-auth-token" {
			t.Error("collector token was not read from the explicit file")
		}
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	path := filepath.Join(t.TempDir(), "collector-token")
	if err := os.WriteFile(path, []byte("fixture-auth-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := options(source.URL)
	o.BearerTokenFile = path
	r, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, span := r.Root(context.Background(), "req_0123456789abcdef0123456789abcdef")
	span.End(OK)
	eventually(t, func() bool { return r.Stats().Failed == 1 })
	if reached.Load() != 0 || r.Stats().Requests != 1 {
		t.Fatal("export followed a redirect or retried an unauthorized destination")
	}
}

func TestExportPolicyStopsBeforeAnyNetworkAttempt(t *testing.T) {
	o := options("http://collector.invalid/v1/traces")
	o.Allowed = func(context.Context) bool { return false }
	r, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, span := r.Root(context.Background(), "req_0123456789abcdef0123456789abcdef")
	span.End(OK)
	eventually(t, func() bool { return r.Stats().Policy == 1 })
	if s := r.Stats(); s.Requests != 0 || s.Failed != 0 || s.Sent != 0 {
		t.Fatalf("policy denial reached network: %+v", s)
	}
}

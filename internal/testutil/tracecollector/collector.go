// Package tracecollector independently checks the small OTLP wire contract in integration fixtures.
package tracecollector

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Frame is a decoded phase, independent of the producer's record and encoding types.
type Frame struct {
	Trace, ID, Parent, Name string
	Start, End              int64
}

// Collector caps retained fixture data and accepts only the exact content-free resource/span schema.
type Collector struct {
	server *httptest.Server
	mu     sync.Mutex
	frames []Frame
	raw    []byte
	err    error
	posts  int
}

func New(t *testing.T) *Collector {
	t.Helper()
	c := &Collector{}
	c.server = httptest.NewServer(http.HandlerFunc(c.receive))
	t.Cleanup(c.server.Close)
	return c
}

func (c *Collector) URL() string { return c.server.URL + "/v1/traces" }

func (c *Collector) receive(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err == nil && (len(b) > 1<<20 || r.Method != "POST" || r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/json") {
		err = errors.New("collector request method, path, content type or size is invalid")
	}
	frames, decodeErr := decode(b)
	if err == nil {
		err = decodeErr
	}
	c.mu.Lock()
	c.posts++
	if err == nil && (len(c.frames)+len(frames) > 50000 || len(c.raw)+len(b) > 16<<20) {
		err = errors.New("collector fixture retention cap exceeded")
	}
	if err != nil {
		if c.err == nil {
			c.err = err
		}
	} else {
		c.frames = append(c.frames, frames...)
		c.raw = append(c.raw, b...)
	}
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
	}
	io.WriteString(w, "{}")
}

func (c *Collector) Snapshot(t *testing.T) ([]Frame, int) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		t.Fatal(c.err)
	}
	return append([]Frame(nil), c.frames...), c.posts
}

func (c *Collector) AssertPrivate(t *testing.T, canaries ...string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		t.Fatal(c.err)
	}
	for _, canary := range canaries {
		if canary != "" && strings.Contains(string(c.raw), canary) {
			t.Fatal("private fixture data entered telemetry")
		}
	}
}

func (c *Collector) Wait(t *testing.T, trace string, names ...string) map[string]Frame {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		found := map[string]Frame{}
		frames, _ := c.Snapshot(t)
		for _, frame := range frames {
			if frame.Trace == trace {
				found[frame.Name] = frame
			}
		}
		complete := true
		for _, name := range names {
			_, ok := found[name]
			complete = complete && ok
		}
		if complete {
			return found
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing correlated trace phases: need %v, received %d", names, len(found))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func objects(raw json.RawMessage) ([]map[string]json.RawMessage, error) {
	var values []map[string]json.RawMessage
	err := json.Unmarshal(raw, &values)
	return values, err
}

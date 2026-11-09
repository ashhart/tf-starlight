package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type contextKey struct{}

type carrier struct {
	trace, span string
	sampled     bool
}

func randomID(bytes int) string {
	b := make([]byte, bytes)
	for {
		_, _ = rand.Read(b)
		s := hex.EncodeToString(b)
		if validID(s, bytes*2) {
			return s
		}
	}
}

func validID(s string, length int) bool {
	if len(s) != length {
		return false
	}
	nonzero := false
	for _, c := range []byte(s) {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
		nonzero = nonzero || c != '0'
	}
	return nonzero
}

func parseParent(s string) (carrier, bool) {
	if len(s) != 55 || s[:3] != "00-" || s[35] != '-' || s[52] != '-' ||
		!validID(s[3:35], 32) || !validID(s[36:52], 16) {
		return carrier{}, false
	}
	for _, c := range []byte(s[53:]) {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return carrier{}, false
		}
	}
	flags, _ := strconv.ParseUint(s[53:], 16, 8)
	return carrier{trace: s[3:35], span: s[36:52], sampled: flags&1 != 0}, true
}

func sampled(trace string, perMillion int) bool {
	if perMillion <= 0 {
		return false
	}
	if perMillion >= 1_000_000 {
		return true
	}
	n, _ := strconv.ParseUint(trace[16:], 16, 64)
	return n%1_000_000 < uint64(perMillion)
}

// Inject carries generated context and its server request ID; baggage and vendor state are excluded.
func Inject(ctx context.Context, h http.Header) {
	c, ok := ctx.Value(contextKey{}).(carrier)
	if !ok {
		return
	}
	flags := "00"
	if c.sampled {
		flags = "01"
	}
	h.Set("Traceparent", "00-"+c.trace+"-"+c.span+"-"+flags)
	h.Set("X-Request-Id", "req_"+c.trace)
	h.Del("Tracestate")
	h.Del("Baggage")
}

// Root reuses only a fresh server-generated req_ ID; public trace context is deliberately restarted.
func (r *Recorder) Root(ctx context.Context, requestID string) (context.Context, *Span) {
	return r.RootAt(ctx, requestID, time.Now())
}

// RootAt includes context setup in a gateway interval whose start was captured on that same local monotonic clock.
func (r *Recorder) RootAt(ctx context.Context, requestID string, start time.Time) (context.Context, *Span) {
	if r == nil {
		return ctx, nil
	}
	id := strings.TrimPrefix(requestID, "req_")
	if !strings.HasPrefix(requestID, "req_") || !validID(id, 32) {
		id = randomID(16)
	}
	c := carrier{trace: id, span: randomID(8), sampled: sampled(id, r.options.SamplePerMillion)}
	ctx = context.WithValue(ctx, contextKey{}, c)
	if !c.sampled {
		return ctx, nil
	}
	span := newSpan(r, c, "", GatewayRequest)
	span.mono, span.wall = start, start.UTC()
	return ctx, span
}

// Accept is used only behind the node's authenticated gateway boundary; local sampling can downsample its parent.
func (r *Recorder) Accept(ctx context.Context, parent string) (context.Context, *Span) {
	if r == nil {
		return ctx, nil
	}
	c, ok := parseParent(parent)
	if !ok {
		c = carrier{trace: randomID(16), sampled: true}
	}
	previous := c.span
	c.span, c.sampled = randomID(8), c.sampled && sampled(c.trace, r.options.SamplePerMillion)
	ctx = context.WithValue(ctx, contextKey{}, c)
	if !c.sampled {
		return ctx, nil
	}
	return ctx, newSpan(r, c, previous, NodeRequest)
}

// Start measures a child on the current local clock; unsampled paths retain propagation without phase allocations.
func (r *Recorder) Start(ctx context.Context, phase Phase) (context.Context, *Span) {
	if r == nil {
		return ctx, nil
	}
	c, ok := ctx.Value(contextKey{}).(carrier)
	if !ok || !c.sampled || !phase.valid() {
		return ctx, nil
	}
	parent := c.span
	c.span = randomID(8)
	return context.WithValue(ctx, contextKey{}, c), newSpan(r, c, parent, phase)
}

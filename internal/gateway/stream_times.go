package gateway

import (
	"io"
	"time"
)

type seenTime struct {
	elapsed time.Duration
	seen    bool
}

// streamTimes contains gateway-local observations; none establishes the client's receive time or token boundaries.
type streamTimes struct {
	start      time.Time
	now        func() time.Time
	data       seenTime
	output     seenTime
	flush      seenTime
	kind       payloadKind
	flushError bool
}

func (t *streamTimes) elapsed() time.Duration {
	if t.now != nil {
		return t.now().Sub(t.start)
	}
	return time.Since(t.start)
}

func (t *streamTimes) event(protocol string, block []byte) bool {
	if t == nil || t.output.seen {
		return false
	}
	kind := modelPayload(protocol, eventData(block))
	if kind == "" {
		return false
	}
	t.output, t.kind = seenTime{elapsed: t.elapsed(), seen: true}, kind
	return true
}

type observedReader struct {
	io.Reader
	times *streamTimes
}

func (r observedReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 && !r.times.data.seen {
		r.times.data = seenTime{elapsed: r.times.elapsed(), seen: true}
	}
	return n, err
}

func (g *Gateway) observeStream(model, route, outcome string, t *streamTimes) {
	if g.Metrics == nil || t == nil {
		return
	}
	m := g.Metrics
	if t.data.seen {
		m.StreamData.WithLabelValues(model, route, outcome).Observe(t.data.elapsed.Seconds())
	}
	if t.output.seen {
		m.ModelOutput.WithLabelValues(model, route, outcome, string(t.kind)).Observe(t.output.elapsed.Seconds())
	}
	if t.flush.seen {
		result := "ok"
		if t.flushError {
			result = "error"
		}
		m.ModelFlush.WithLabelValues(model, route, outcome, result).Observe(t.flush.elapsed.Seconds())
	}
}

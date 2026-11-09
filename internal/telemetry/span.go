package telemetry

import (
	"context"
	"sync/atomic"
	"time"
)

// Span retains fixed identifiers and local clocks only; no request, header, body or error object is stored.
type Span struct {
	recorder *Recorder
	carrier  carrier
	parent   string
	phase    Phase
	mono     time.Time
	wall     time.Time
	ended    atomic.Bool
}

type record struct {
	TraceID      string `json:"traceId"`
	SpanID       string `json:"spanId"`
	ParentSpanID string `json:"parentSpanId,omitempty"`
	Name         string `json:"name"`
	Kind         int    `json:"kind"`
	Flags        int    `json:"flags"`
	Start        int64  `json:"startTimeUnixNano,string"`
	End          int64  `json:"endTimeUnixNano,string"`
	Status       status `json:"status"`
}

type status struct {
	Code Result `json:"code"`
}

func newSpan(r *Recorder, c carrier, parent string, phase Phase) *Span {
	now := time.Now()
	return &Span{recorder: r, carrier: c, parent: parent, phase: phase, mono: now, wall: now.UTC()}
}

// End is safe on nil and commits one record at most, using elapsed monotonic time rather than the end wall clock.
func (s *Span) End(result Result) {
	if s == nil || s.ended.Swap(true) {
		return
	}
	s.offer(time.Since(s.mono), result)
}

func (s *Span) offer(elapsed time.Duration, result Result) {
	end := s.wall.Add(elapsed).UnixNano()
	if elapsed < 0 || result > Error || !s.phase.valid() || s.wall.UnixNano() <= 0 || end < s.wall.UnixNano() {
		s.recorder.stats.invalid.Add(1)
		return
	}
	s.recorder.offer(record{TraceID: s.carrier.trace, SpanID: s.carrier.span, ParentSpanID: s.parent,
		Name: phaseNames[s.phase], Kind: s.phase.kind(), Flags: 1, Start: s.wall.UnixNano(),
		End: end, Status: status{Code: result}})
}

// Boundary records an observed gateway boundary on its original local clock without inspecting data.
func (r *Recorder) Boundary(ctx context.Context, phase Phase, start time.Time, elapsed time.Duration) {
	if phase != GatewayFirstData && phase != GatewayFirstOutput && phase != GatewayFirstFlush {
		return
	}
	_, span := r.Start(ctx, phase)
	if span != nil {
		span.wall = start.UTC()
		span.offer(elapsed, Unset)
	}
}

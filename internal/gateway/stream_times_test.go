package gateway

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/httpx"
	"github.com/ashhart/tf-starlight/internal/metrics"
	"github.com/ashhart/tf-starlight/internal/providers"
)

type streamClock struct{ at time.Duration }

func (c *streamClock) now() time.Time { return time.Unix(0, 0).Add(c.at) }

type readStep struct {
	at   time.Duration
	data string
	err  error
}

type timedReader struct {
	clock *streamClock
	steps []readStep
}

func (r *timedReader) Read(p []byte) (int, error) {
	if len(r.steps) == 0 {
		return 0, io.EOF
	}
	s := &r.steps[0]
	if s.at > r.clock.at {
		r.clock.at = s.at
	}
	n := copy(p, s.data)
	s.data = s.data[n:]
	if s.data != "" {
		return n, nil
	}
	err := s.err
	r.steps = r.steps[1:]
	return n, err
}

type timedWriter struct {
	*httptest.ResponseRecorder
	clock     *streamClock
	writeErr  error
	flushErr  error
	writeCost time.Duration
	flushCost time.Duration
}

func (w timedWriter) Write(p []byte) (int, error) {
	w.clock.at += w.writeCost
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.ResponseRecorder.Write(p)
}

func (w timedWriter) FlushError() error {
	w.clock.at += w.flushCost
	w.ResponseRecorder.Flush()
	return w.flushErr
}

func TestStreamClocksIgnoreMetadataAndSeparateReceiveFromFlush(t *testing.T) {
	clock := &streamClock{}
	times := streamTimes{start: clock.now(), now: clock.now}
	steps := []readStep{
		{at: 10 * time.Millisecond, data: ": ping\n\n"},
		{at: 20 * time.Millisecond, data: "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"},
		{at: 30 * time.Millisecond, data: "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"call_a\"}]}}]}\n\n"},
		{at: 50 * time.Millisecond, data: "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{\"}}]}}]}\n\n"},
		{at: 80 * time.Millisecond, data: "data: {\"choices\":[{\"delta\":{\"content\":\"later\"}}]}\n\n"},
		{at: 90 * time.Millisecond, data: "data: {\"choices\":[],\"usage\":{\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"},
	}
	var wire strings.Builder
	for _, step := range steps {
		wire.WriteString(step.data)
	}
	w := timedWriter{ResponseRecorder: httptest.NewRecorder(), clock: clock, writeCost: 2 * time.Millisecond,
		flushCost: 3 * time.Millisecond}
	m := &meter{protocol: providers.ChatCompletions}
	outcome := stream(context.Background(), w, &timedReader{clock: clock, steps: steps}, m, false, httpx.OpenAI, &times)
	if outcome != "ok" || !m.usage.Observed {
		t.Fatalf("outcome %q, usage %+v", outcome, m.usage)
	}
	if times.data.elapsed != 10*time.Millisecond || times.output.elapsed != 50*time.Millisecond {
		t.Fatalf("data/output clocks: %+v", times)
	}
	if times.kind != toolPayload || times.flush.elapsed != 52*time.Millisecond || times.flushError {
		t.Fatalf("first output/flush clocks: %+v", times)
	}
	if w.Body.String() != wire.String() {
		t.Fatalf("observer changed stream bytes: got %q, want %q", w.Body.String(), wire.String())
	}
}

func TestStreamClocksRequireACompleteMeaningfulEvent(t *testing.T) {
	for _, body := range []string{
		": ping\n\ndata: [DONE]\n\n",
		"data: {\"choices\":[],\"usage\":{\"completion_tokens\":2}}\n\n",
		"data: {\"error\":{\"message\":\"failed\"}}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"truncated\"}}]}",
		"data: {\"choices\":[\n\n",
	} {
		t.Run(body, func(t *testing.T) {
			times := streamTimes{start: time.Now()}
			stream(context.Background(), httptest.NewRecorder(), strings.NewReader(body),
				&meter{protocol: providers.ChatCompletions}, true, httpx.OpenAI, &times)
			if !times.data.seen || times.output.seen || times.flush.seen {
				t.Fatalf("metadata or partial event qualified as output: %+v", times)
			}
		})
	}
}

func TestStreamClocksDistinguishWriteAndFlushFailures(t *testing.T) {
	for _, failWrite := range []bool{false, true} {
		clock := &streamClock{}
		times := streamTimes{start: clock.now(), now: clock.now}
		w := timedWriter{ResponseRecorder: httptest.NewRecorder(), clock: clock, flushErr: errors.New("flush failed")}
		if failWrite {
			w.writeErr = errors.New("client left")
		}
		body := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n"
		outcome := stream(context.Background(), w, strings.NewReader(body), &meter{protocol: providers.ChatCompletions},
			false, httpx.OpenAI, &times)
		if !times.output.seen || times.flush.seen == failWrite || times.flushError == failWrite {
			t.Fatalf("write failure %v: %+v", failWrite, times)
		}
		if failWrite && outcome != "cancelled" {
			t.Fatalf("write failure outcome %q", outcome)
		}
	}
}

func TestStreamClocksObserveMultilineEventsWithoutChangingThem(t *testing.T) {
	body := "event: chunk\r\ndata: {\"choices\":\r\ndata: [{\"delta\":{\"content\":\"hi\"}}]}\r\n\r\n"
	w := httptest.NewRecorder()
	times := streamTimes{start: time.Now()}
	stream(context.Background(), w, strings.NewReader(body), &meter{protocol: providers.ChatCompletions}, false,
		httpx.OpenAI, &times)
	if !times.output.seen || w.Body.String() != body {
		t.Fatalf("timing %+v, body %q", times, w.Body.String())
	}
}

func TestStreamCancellationBeforeOutputRemainsUnobserved(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	clock := &streamClock{}
	times := streamTimes{start: clock.now(), now: clock.now}
	body := &timedReader{clock: clock, steps: []readStep{
		{at: time.Millisecond, data: ": ping\n\n"},
		{at: 2 * time.Millisecond, err: context.Canceled},
	}}
	outcome := stream(ctx, httptest.NewRecorder(), body, &meter{protocol: providers.ChatCompletions}, false,
		httpx.OpenAI, &times)
	if outcome != "cancelled" || !times.data.seen || times.output.seen || times.flush.seen {
		t.Fatalf("cancelled stream timing %+v, outcome %q", times, outcome)
	}
}

type headerWriter struct {
	*httptest.ResponseRecorder
	headerAt time.Time
}

func (w *headerWriter) WriteHeader(code int) {
	w.headerAt = time.Now()
	w.ResponseRecorder.WriteHeader(code)
}

type headerReader struct {
	reader *strings.Reader
	writer *headerWriter
	t      *testing.T
	readAt time.Time
}

func (r *headerReader) Read(p []byte) (int, error) {
	if r.readAt.IsZero() {
		if r.writer.headerAt.IsZero() || r.writer.Flushed {
			r.t.Fatal("upstream read did not follow the unflushed response header stage")
		}
		r.readAt = time.Now()
	}
	return r.reader.Read(p)
}

func TestRelayKeepsLegacyHeaderBoundarySeparateFromModelOutput(t *testing.T) {
	resp := sse(`data: {"choices":[{"delta":{"content":"hi"}}]}`, `data: [DONE]`)
	w := &headerWriter{ResponseRecorder: httptest.NewRecorder()}
	r := &headerReader{reader: strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"), writer: w, t: t}
	resp.Body = io.NopCloser(r)
	start := time.Now()
	out := relay(context.Background(), w, resp, providers.ChatCompletions, false, httpx.OpenAI, start, true)
	if !out.times.data.seen || !out.times.output.seen || !out.times.flush.seen {
		t.Fatalf("stream timing missing: %+v", out.times)
	}
	if out.firstByte > r.readAt.Sub(start) || out.times.data.elapsed < r.readAt.Sub(start) {
		t.Fatalf("header and data boundary order: legacy %v, read %v, times %+v", out.firstByte, r.readAt.Sub(start), out.times)
	}
	if out.times.output.elapsed < out.times.data.elapsed || out.times.flush.elapsed < out.times.output.elapsed {
		t.Fatalf("stream phase order: %+v", out.times)
	}
}

func TestRelayStreamObservationCanBeDisabled(t *testing.T) {
	resp := sse(`data: {"choices":[{"delta":{"content":"hi"}}]}`, `data: [DONE]`)
	w := httptest.NewRecorder()
	out := relay(context.Background(), w, resp, providers.ChatCompletions, false, httpx.OpenAI, time.Now(), false)
	if out.times != nil || !strings.Contains(w.Body.String(), `"hi"`) {
		t.Fatalf("disabled observation changed the relay: %+v, body %q", out.times, w.Body.String())
	}
}

func TestStreamMetricsKeepUnknownDifferentFromAnObservedZero(t *testing.T) {
	g := &Gateway{Metrics: metrics.New()}
	g.observeStream("model", "local", "cancelled", &streamTimes{})
	families, err := g.Metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if strings.HasPrefix(family.GetName(), "starlight_first_model_output") {
			t.Fatal("unobserved model output created a metric sample")
		}
	}
	g.observeStream("model", "local", "cancelled", &streamTimes{output: seenTime{seen: true}, kind: toolPayload})
	families, err = g.Metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, family := range families {
		if family.GetName() == "starlight_first_model_output_seconds" {
			found = true
			if len(family.Metric) != 1 || family.Metric[0].Histogram.GetSampleCount() != 1 || family.Metric[0].Histogram.GetSampleSum() != 0 {
				t.Fatalf("observed zero was lost: %+v", family)
			}
		}
	}
	if !found {
		t.Fatal("observed output was not exported")
	}
}

func BenchmarkStreamObservation(b *testing.B) {
	body := ": ping\n\ndata: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{\"}}]}}]}\n\n"
	for _, observed := range []bool{false, true} {
		name := "unobserved"
		if observed {
			name = "observed"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var times *streamTimes
				if observed {
					times = &streamTimes{start: time.Now()}
				}
				stream(context.Background(), httptest.NewRecorder(), strings.NewReader(body),
					&meter{protocol: providers.ChatCompletions}, false, httpx.OpenAI, times)
			}
		})
	}
}

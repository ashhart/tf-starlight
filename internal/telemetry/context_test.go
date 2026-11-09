package telemetry

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func pureRecorder() *Recorder {
	return &Recorder{options: Options{SamplePerMillion: 1_000_000}, queue: make(chan record, 32)}
}

func TestTrustedRootAndChildContainOnlyFixedContext(t *testing.T) {
	r := pureRecorder()
	id := "0123456789abcdef0123456789abcdef"
	ctx, root := r.Root(context.Background(), "req_"+id)
	ctx, child := r.Start(ctx, GatewayDispatch)
	h := http.Header{"Tracestate": {"private-vendor-data"}, "Baggage": {"private-caller-data"}}
	Inject(ctx, h)
	parent, ok := parseParent(h.Get("Traceparent"))
	if !ok || parent.trace != id || !parent.sampled || h.Get("X-Request-Id") != "req_"+id ||
		h.Get("Tracestate") != "" || h.Get("Baggage") != "" {
		t.Fatalf("unsafe propagation: %v", h)
	}
	child.End(OK)
	child.End(Error)
	root.End(Unset)
	if len(r.queue) != 2 {
		t.Fatal("end was not once-only")
	}
	a, b := <-r.queue, <-r.queue
	if a.ParentSpanID != b.SpanID || a.SpanID != parent.span || a.TraceID != b.TraceID || a.Name != "gateway.dispatch" {
		t.Fatalf("broken parent/child records: %+v %+v", a, b)
	}
	_, node := r.Accept(context.Background(), h.Get("Traceparent"))
	node.End(Unset)
	n := <-r.queue
	if n.TraceID != a.TraceID || n.ParentSpanID != a.SpanID || n.Name != "node.request" {
		t.Fatalf("node lost the authenticated gateway parent: %+v", n)
	}
}

func TestParentValidationAndLocalSampling(t *testing.T) {
	valid := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-09"
	if c, ok := parseParent(valid); !ok || !c.sampled {
		t.Fatal("sampled flag was compared as a value instead of a bit")
	}
	for _, bad := range []string{"", valid + "-extra", "01" + valid[2:], strings.ToUpper(valid),
		"00-" + strings.Repeat("0", 32) + valid[35:], valid[:36] + strings.Repeat("0", 16) + "-01",
		valid[:53] + "zz", "private raw context"} {
		if _, ok := parseParent(bad); ok {
			t.Fatal("malformed context accepted")
		}
	}
	r := pureRecorder()
	r.options.SamplePerMillion = 0
	ctx, root := r.Root(context.Background(), "req_0123456789abcdef0123456789abcdef")
	_, child := r.Start(ctx, GatewayAdmission)
	_, node := r.Accept(ctx, valid)
	h := http.Header{}
	Inject(ctx, h)
	if root != nil || child != nil || node != nil || !strings.HasSuffix(h.Get("Traceparent"), "-00") || len(r.queue) != 0 {
		t.Fatal("zero sampling recorded work or honored a remote request to raise sampling")
	}
}

func TestLocalDurationSurvivesWallClockSkew(t *testing.T) {
	r := pureRecorder()
	ctx, root := r.Root(context.Background(), "req_0123456789abcdef0123456789abcdef")
	root.wall = root.wall.Add(36 * time.Hour)
	time.Sleep(5 * time.Millisecond)
	root.End(Unset)
	span := <-r.queue
	if elapsed := time.Duration(span.End - span.Start); elapsed < 5*time.Millisecond || elapsed > time.Second {
		t.Fatalf("wall clock skew became a local duration: %v", elapsed)
	}
	start := time.Now()
	r.Boundary(ctx, GatewayFirstOutput, start, 25*time.Millisecond)
	boundary := <-r.queue
	if boundary.Start != start.UnixNano() || boundary.End-boundary.Start != int64(25*time.Millisecond) {
		t.Fatalf("cumulative boundary used another clock: %+v", boundary)
	}
}

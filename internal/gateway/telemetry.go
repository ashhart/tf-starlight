package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/ashhart/tf-starlight/internal/httpx"
	"github.com/ashhart/tf-starlight/internal/telemetry"
)

type phaseTracer struct{ *telemetry.Recorder }

const (
	admissionPhase  = telemetry.GatewayAdmission
	dispatchPhase   = telemetry.GatewayDispatch
	relayPhase      = telemetry.GatewayRelay
	settlementPhase = telemetry.GatewaySettlement
	traceUnset      = telemetry.Unset
)

// SetTelemetry attaches the explicit producer during startup, before serving requests.
func (g *Gateway) SetTelemetry(r *telemetry.Recorder) {
	if r == nil {
		g.traces = nil
		return
	}
	g.traces = &phaseTracer{r}
}

func (g *Gateway) startTrace(ctx context.Context, phase telemetry.Phase) (context.Context, *telemetry.Span) {
	if g.traces == nil {
		return ctx, nil
	}
	return g.traces.Start(ctx, phase)
}

func (g *Gateway) traceRequest(r *http.Request, start time.Time) (*http.Request, *telemetry.Span, *telemetry.Span) {
	if g.traces == nil {
		return r, nil, nil
	}
	ctx, request := g.traces.RootAt(r.Context(), httpx.RequestID(r.Context()), start)
	_, preparation := g.traces.Start(ctx, telemetry.GatewayPrepare)
	return r.WithContext(ctx), request, preparation
}

func dispatchResult(response *http.Response, err error) telemetry.Result {
	if err != nil || response == nil || response.StatusCode >= 400 {
		return telemetry.Error
	}
	return telemetry.OK
}

func traceResult(err error) telemetry.Result {
	if err != nil {
		return telemetry.Error
	}
	return telemetry.OK
}

func (g *Gateway) traceBoundaries(ctx context.Context, start time.Time, t streamTimes) {
	if g.traces == nil {
		return
	}
	if t.data.seen {
		g.traces.Boundary(ctx, telemetry.GatewayFirstData, start, t.data.elapsed)
	}
	if t.output.seen {
		g.traces.Boundary(ctx, telemetry.GatewayFirstOutput, start, t.output.elapsed)
	}
	if t.flush.seen {
		g.traces.Boundary(ctx, telemetry.GatewayFirstFlush, start, t.flush.elapsed)
	}
}

package proxy

import (
	"net/http"

	"github.com/ashhart/tf-starlight/internal/telemetry"
)

func (s *Server) traceRequest(r *http.Request) (*http.Request, *telemetry.Span, *telemetry.Span) {
	if s.Telemetry == nil {
		return r, nil, nil
	}
	ctx, request := s.Telemetry.Accept(r.Context(), r.Header.Get("Traceparent"))
	_, permit := s.Telemetry.Start(ctx, telemetry.NodePermit)
	return r.WithContext(ctx), request, permit
}

func headersResult(resp *http.Response, err error) telemetry.Result {
	if err != nil || resp == nil || resp.StatusCode >= 400 {
		return telemetry.Error
	}
	return telemetry.OK
}

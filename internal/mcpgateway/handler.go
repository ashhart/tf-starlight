package mcpgateway

import (
	"context"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"time"

	"github.com/ashhart/tf-starlight/internal/config"
	"github.com/ashhart/tf-starlight/internal/identity/principal"
	"github.com/ashhart/tf-starlight/internal/ids"
)

// Event accounts for tool transport separately from inference tokens, without arguments or results.
type Event struct {
	AttemptID        string
	Status           int
	Actor            *principal.Principal
	Method           string
	Tool             string
	Phase            string
	Outcome          string
	DispatchPossible bool
	ElapsedMS        int64
	RefusalCounts    map[string]uint64
}

// Options supplies required installation policy, authorization and durable accounting.
type Options struct {
	Config    config.MCP
	PublicURL string
	Offline   func() bool
	// Authorize checks a nil body before capacity, then a parsed body before dispatch.
	Authorize func(context.Context, *http.Request, []byte) (*principal.Principal, int)
	Record    func(context.Context, Event) error
	Log       *slog.Logger
}

// Handler is one bounded tool route with an immediate-rejection per-instance concurrency limit.
type Handler struct {
	options  Options
	upstream *upstream
	allowed  map[string]bool
	origin   string
	slots    chan struct{}
	refusals *refusalSummary
}

// New constructs a gateway only for a complete explicit grant.
func New(o Options) (*Handler, error) {
	if !o.Config.Enabled || o.Authorize == nil || o.Record == nil || o.Offline == nil {
		return nil, errors.New("MCP gateway requires enabled policy, authorization and accounting")
	}
	if err := o.Config.Validate(); err != nil {
		return nil, err
	}
	u, err := url.Parse(o.PublicURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("MCP public origin is invalid")
	}
	bearer, err := o.Config.Bearer()
	if err != nil {
		return nil, err
	}
	up, err := newUpstream(o.Config.Endpoint, bearer, o.Config.Internal, o.Offline)
	if err != nil {
		return nil, err
	}
	up.Timeout = o.Config.Timeout.Duration
	h := &Handler{options: o, upstream: up, origin: u.Scheme + "://" + u.Host,
		allowed: map[string]bool{}, slots: make(chan struct{}, o.Config.MaxConcurrent)}
	for _, tool := range o.Config.Tools {
		h.allowed[tool] = true
	}
	h.refusals = newRefusalSummary(o.Record, o.Log, time.Minute)
	return h, nil
}

// Close flushes anonymous counters and releases only this gateway's idle upstream connections.
func (h *Handler) Close() {
	h.refusals.close()
	h.upstream.HTTP.CloseIdleConnections()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	event := Event{AttemptID: ids.New(), Method: "unknown"}
	w.Header().Set("X-Starlight-MCP-Attempt-ID", event.AttemptID)
	deadline := time.Now().Add(h.options.Config.Timeout.Duration)
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
	defer func() {
		if time.Now().Before(deadline) {
			_ = rc.SetReadDeadline(time.Time{})
			_ = rc.SetWriteDeadline(time.Time{})
		}
	}()
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	r = r.WithContext(ctx)
	if origins := r.Header.Values("Origin"); len(origins) != 0 && (len(origins) != 1 || origins[0] != h.origin) {
		h.reject(w, r, nil, &failure{403, -32600, "Origin is not permitted"}, event)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		h.reject(w, r, nil, &failure{405, -32600, "Use POST"}, event)
		return
	}
	actor, status := h.options.Authorize(ctx, r, nil)
	event.Actor = actor
	if status != 0 || actor == nil {
		if status == 0 {
			status = http.StatusForbidden
		}
		h.reject(w, r, nil, &failure{status, -32000, "Tool authority is unavailable or denied"}, event)
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		h.reject(w, r, nil, &failure{429, -32000, "Tool gateway is at capacity"}, event)
		return
	}
	raw, err := readBounded(r.Body, 64<<10)
	if err != nil {
		h.reject(w, r, nil, &failure{413, -32600, "Request body is incomplete or exceeds its bound"}, event)
		return
	}
	req, problem := parseRequest(r.Header, raw)
	if problem != nil {
		h.reject(w, r, req.ID, problem, event)
		return
	}
	event.Method = req.Method
	if h.allowed[req.name] {
		event.Tool = req.name
	}
	actor, status = h.options.Authorize(ctx, r, raw)
	if status != 0 || actor == nil {
		if status == 0 {
			status = http.StatusForbidden
		}
		h.reject(w, r, req.ID, &failure{status, -32000, "Tool authority is unavailable or denied"}, event)
		return
	}
	event.Actor = actor
	if req.Method == "tools/call" && !h.allowed[req.name] {
		h.reject(w, r, req.ID, &failure{403, -32602, "Tool is not permitted"}, event)
		return
	}
	if req.Method == "server/discover" {
		writeDiscovery(w, req.ID)
		return
	}
	h.forward(w, r, req, event, raw)
}

func (h *Handler) forward(w http.ResponseWriter, r *http.Request, req request, event Event, raw []byte) {
	event.Phase, event.Outcome = "begin", "authorized"
	if err := h.options.Record(r.Context(), event); err != nil {
		writeFailure(w, req.ID, &failure{503, -32000, "Tool accounting is unavailable"})
		return
	}
	started := time.Now()
	event.Phase, event.Outcome = "end", "upstream_incomplete"
	defer func() {
		event.ElapsedMS = time.Since(started).Milliseconds()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
		defer cancel()
		if err := h.options.Record(ctx, event); err != nil && h.options.Log != nil {
			h.options.Log.Error("MCP completion accounting unavailable")
		}
	}()
	x, err := h.upstream.send(r.Context(), r.Header, raw)
	event.DispatchPossible = x.dispatched
	defer x.close()
	if err != nil || x.response.StatusCode != http.StatusOK {
		writeFailure(w, req.ID, &failure{502, -32000, "Tool upstream did not complete; execution may be unknown"})
		return
	}
	kind, _, err := mime.ParseMediaType(x.response.Header.Get("Content-Type"))
	if err == nil && kind == "text/event-stream" {
		started, relayErr := relayEvents(r.Context(), w, x.response.Body, req, h.allowed, h.upstream.MaxBytes, h.upstream.MaxFrame, h.upstream.Bearer)
		if relayErr != nil {
			if !started {
				writeFailure(w, req.ID, &failure{502, -32000, "Tool upstream stream did not complete"})
			}
			return
		}
	} else if err == nil && kind == "application/json" {
		body, readErr := readBounded(x.response.Body, h.upstream.MaxBytes)
		validated, final, validateErr := response(body, req, h.allowed, int(h.upstream.MaxBytes), h.upstream.Bearer)
		if readErr != nil || validateErr != nil || !final {
			writeFailure(w, req.ID, &failure{502, -32000, "Tool upstream returned an invalid final response"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err = w.Write(validated); err != nil {
			return
		}
	} else {
		writeFailure(w, req.ID, &failure{502, -32000, "Tool upstream response type is unsupported"})
		return
	}
	event.Outcome = "response_complete"
}

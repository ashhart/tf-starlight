package mcpgateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ashhart/tf-starlight/internal/providers"
)

// upstream is one fixed endpoint and a separate credential, never a caller-selected address or bearer token.
type upstream struct {
	URL       *url.URL
	Bearer    string
	HTTP      *http.Client
	Timeout   time.Duration
	IsOffline func() bool
	MaxBytes  int64
	MaxFrame  int
}

type exchange struct {
	response   *http.Response
	dispatched bool
	cancel     context.CancelFunc
}

func newUpstream(endpoint, bearer string, internal bool, offline func() bool) (*upstream, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && !(internal && u.Scheme == "http")) {
		return nil, errors.New("MCP endpoint must be an approved absolute HTTP URL without credentials or query")
	}
	if strings.ContainsAny(bearer, "\r\n") {
		return nil, errors.New("MCP upstream credential is invalid")
	}
	egress := providers.NewDirectEgress()
	egress.Allow(map[string]bool{u.Hostname(): internal}, false)
	return &upstream{URL: u, Bearer: bearer, HTTP: egress.Client(), Timeout: 30 * time.Second,
		IsOffline: offline, MaxBytes: 8 << 20, MaxFrame: 1 << 20}, nil
}

// send performs one request; ambiguity after any possible dispatch is never an invitation to retry.
func (u *upstream) send(ctx context.Context, original http.Header, raw []byte) (exchange, error) {
	if u.IsOffline == nil || u.IsOffline() {
		return exchange{}, providers.ErrOffline
	}
	if _, problem := parseRequest(original, raw); problem != nil {
		return exchange{}, errors.New("invalid MCP request")
	}
	ctx, cancel := context.WithTimeout(ctx, u.Timeout)
	var sent atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn:      func(httptrace.GotConnInfo) { sent.Store(true) },
		WroteRequest: func(httptrace.WroteRequestInfo) { sent.Store(true) },
	})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, u.URL.String(), bytes.NewReader(raw))
	if err != nil {
		cancel()
		return exchange{}, errors.New("invalid MCP upstream request")
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	for key, values := range original {
		lower := strings.ToLower(key)
		if lower != "mcp-protocol-version" && lower != "mcp-method" && lower != "mcp-name" && !strings.HasPrefix(lower, "mcp-param-") {
			continue
		}
		for _, value := range values {
			r.Header.Add(key, value)
		}
	}
	if u.Bearer != "" {
		r.Header.Set("Authorization", "Bearer "+u.Bearer)
	}
	response, err := u.HTTP.Do(r)
	result := exchange{response: response, dispatched: sent.Load() || response != nil, cancel: cancel}
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		cancel()
		return result, errors.New("MCP upstream did not return a complete response")
	}
	return result, nil
}

func (x *exchange) close() {
	if x.response != nil {
		_ = x.response.Body.Close()
	}
	if x.cancel != nil {
		x.cancel()
	}
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("MCP response is incomplete or exceeds the configured bound")
	}
	return raw, nil
}

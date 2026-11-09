package server

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ashhart/tf-starlight/internal/audit"
	"github.com/ashhart/tf-starlight/internal/authn"
	"github.com/ashhart/tf-starlight/internal/authz"
	"github.com/ashhart/tf-starlight/internal/crypto/secret"
	"github.com/ashhart/tf-starlight/internal/db"
	"github.com/ashhart/tf-starlight/internal/gateway"
	"github.com/ashhart/tf-starlight/internal/guard"
	"github.com/ashhart/tf-starlight/internal/httpx"
	"github.com/ashhart/tf-starlight/internal/identity/principal"
	"github.com/ashhart/tf-starlight/internal/levels"
	"github.com/ashhart/tf-starlight/internal/mcpgateway"
)

func (t *Starlight) buildMCP(proxies *httpx.Proxies) error {
	cfg := t.Config.MCP
	if !cfg.Enabled || !t.Config.HasRole("gateway") {
		return nil
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	keys := map[string]bool{}
	for _, id := range cfg.KeyIDs {
		keys[id] = true
	}
	ceiling, _ := levels.ParseClass(cfg.MaxClass)
	auth := &authn.Authenticator{Pool: t.Pool, TTL: time.Nanosecond, Timeout: db.CallTimeout}
	policies := &gateway.Policies{Pool: t.Pool, TTL: time.Nanosecond}
	offline := func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), db.CallTimeout)
		defer cancel()
		return policies.Offline(ctx)
	}
	authorize := func(ctx context.Context, r *http.Request, raw []byte) (*principal.Principal, int) {
		values := r.Header.Values("Authorization")
		if len(values) != 1 || r.Header.Get("X-Api-Key") != "" {
			return nil, http.StatusUnauthorized
		}
		scheme, token, ok := strings.Cut(values[0], " ")
		kind, parsed := secret.Parse(token)
		if !ok || !strings.EqualFold(scheme, "Bearer") || !parsed || kind != secret.APIKey {
			return nil, http.StatusUnauthorized
		}
		caller, err := auth.Authenticate(ctx, token, cfg.ProjectID, proxies.ClientIP(r))
		if err != nil {
			if errors.Is(err, authn.ErrInvalid) {
				return nil, http.StatusUnauthorized
			}
			if errors.Is(err, authn.ErrProject) {
				return nil, http.StatusForbidden
			}
			return nil, http.StatusServiceUnavailable
		}
		if caller.P.Via != principal.ViaAPIKey || caller.P.ProjectID != cfg.ProjectID || !keys[caller.P.KeyID] ||
			!caller.Grants.Can(authz.Inference, cfg.ProjectID) {
			return nil, http.StatusForbidden
		}
		if hint := r.Header.Get("X-Starlight-Project"); hint != "" && hint != cfg.ProjectID {
			return nil, http.StatusForbidden
		}
		policy, err := policies.Project(ctx, cfg.ProjectID)
		if err != nil {
			return nil, http.StatusServiceUnavailable
		}
		class := levels.Max(policy.Class, caller.ClassFloor)
		if raised := r.Header.Get("X-Starlight-Classification"); raised != "" {
			asked, err := levels.ParseClass(raised)
			if err != nil {
				return nil, http.StatusBadRequest
			}
			class = levels.Max(class, asked)
		}
		if policy.Isolated && !cfg.Internal || class > ceiling {
			return nil, http.StatusForbidden
		}
		if policies.Offline(ctx) {
			return nil, http.StatusServiceUnavailable
		}
		if policy.SecretsGuard != "off" && mcpHasCredentials(r, raw) && (policy.SecretsGuard == "block" || !cfg.Internal) {
			return nil, http.StatusForbidden
		}
		return caller.P, 0
	}
	record := func(ctx context.Context, e mcpgateway.Event) error {
		return audit.Log(ctx, t.Pool, audit.Event{ActorType: string(e.Actor.Type), ActorID: e.Actor.ID,
			Action: "mcp." + e.Phase, TargetType: "project", TargetID: cfg.ProjectID, RequestID: httpx.RequestID(ctx),
			Outcome: e.Outcome, Details: map[string]any{"key_id": e.Actor.KeyID, "method": e.Method, "tool": e.Tool,
				"dispatch_possible": e.DispatchPossible, "elapsed_ms": e.ElapsedMS}})
	}
	var err error
	t.MCP, err = mcpgateway.New(mcpgateway.Options{Config: cfg, PublicURL: t.Config.PublicURL,
		Offline: offline, Authorize: authorize, Record: record, Log: t.Log})
	return err
}

func mcpHasCredentials(r *http.Request, raw []byte) bool {
	if len(guard.ScanJSON(raw)) != 0 {
		return true
	}
	for key, values := range r.Header {
		if !strings.HasPrefix(strings.ToLower(key), "mcp-param-") {
			continue
		}
		for _, value := range values {
			if strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=") {
				if decoded, err := base64.StdEncoding.DecodeString(value[9 : len(value)-2]); err == nil {
					value = string(decoded)
				}
			}
			if len(guard.Scan([]byte(value))) != 0 {
				return true
			}
		}
	}
	return false
}

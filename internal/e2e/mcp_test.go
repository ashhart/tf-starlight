package e2e_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/audit"
	"github.com/ashhart/tf-starlight/internal/config"
	"github.com/ashhart/tf-starlight/internal/crypto/keyring"
	"github.com/ashhart/tf-starlight/internal/crypto/secret"
	"github.com/ashhart/tf-starlight/internal/db"
	"github.com/ashhart/tf-starlight/internal/identity/directory"
	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/server"
	"github.com/ashhart/tf-starlight/internal/testutil"
	"github.com/jackc/pgx/v5"
)

func TestGovernedMCPRouteUsesFreshAuthorityAndSeparateAccounting(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	master := keyring.GenerateMasterKey()
	m, _ := keyring.LoadMaster(master, "")
	if err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		_, _, err := server.Init(ctx, tx, keyring.New(m), server.InitOptions{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	user, err := directory.CreateUser(ctx, pool, directory.NewUser{Email: "tools@example.com", Source: "jit"})
	if err != nil {
		t.Fatal(err)
	}
	project, keyID := ids.New(), ids.New()
	exec(t, pool, "INSERT INTO projects(id,slug,name) VALUES($1,'tools','Tools')", project)
	exec(t, pool, "INSERT INTO role_bindings(id,role,subject_type,subject_id,project_id) VALUES($1,'member','user',$2,$3)", ids.New(), user.ID, project)
	key, hash := secret.Generate(secret.APIKey)
	exec(t, pool, `INSERT INTO api_keys(id,public_id,secret_hash,name,owner_type,owner_id,project_id)
		VALUES($1,$2,$3,'tools','user',$4,$5)`, keyID, secret.PublicID(key), hash, user.ID, project)
	ungranted, ungrantedHash := secret.Generate(secret.APIKey)
	exec(t, pool, `INSERT INTO api_keys(id,public_id,secret_hash,name,owner_type,owner_id,project_id)
		VALUES($1,$2,$3,'ungranted','user',$4,$5)`, ids.New(), secret.PublicID(ungranted), ungrantedHash, user.ID, project)
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fixture-upstream" || r.Header.Get("X-Api-Key") != "" {
			t.Error("caller credential crossed upstream boundary")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","content":[{"type":"text","text":"private-result"}]}}`)
	}))
	defer up.Close()
	t.Setenv("STARLIGHT_MCP_E2E_FIXTURE", "fixture-upstream")
	cfg := config.Defaults()
	cfg.PublicURL = "http://127.0.0.1:8443"
	cfg.Database.URL = pool.Config().ConnString()
	cfg.Keys.MasterKey = master
	cfg.MCP = config.MCP{Enabled: true, Endpoint: up.URL, Internal: true, ProjectID: project, KeyIDs: []string{keyID}, Tools: []string{"echo"},
		MaxClass: "internal", MaxConcurrent: 1, Timeout: config.Duration{Duration: 2 * time.Second}, BearerEnv: "STARLIGHT_MCP_E2E_FIXTURE"}
	app, err := server.Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	public := httptest.NewServer(app.Handler())
	defer public.Close()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
		"name": "echo", "arguments": map[string]string{"text": "private-argument"}, "_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientInfo": map[string]string{"name": "fixture", "version": "1"},
			"io.modelcontextprotocol/clientCapabilities": map[string]any{}}}})
	call := func(token, raise string, extra ...string) int {
		r, _ := http.NewRequest("POST", public.URL+"/mcp", strings.NewReader(string(body)))
		for k, v := range map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json", "Accept": "application/json, text/event-stream",
			"Mcp-Method": "tools/call", "Mcp-Name": "echo", "MCP-Protocol-Version": "2026-07-28", "X-Starlight-Classification": raise} {
			r.Header.Set(k, v)
		}
		for i := 0; i < len(extra); i += 2 {
			r.Header.Set(extra[i], extra[i+1])
		}
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
		return res.StatusCode
	}
	if status := call(key, ""); status != 200 || calls.Load() != 1 {
		t.Fatalf("permitted call status=%d calls=%d", status, calls.Load())
	}
	for _, fixture := range []struct {
		name, sql, undo string
		args            []any
		token, raise    string
		want            int
	}{
		{"classification", "", "", nil, key, "restricted", 403},
		{"non-key", "", "", nil, "not-a-key", "", 401},
		{"inference key without tool grant", "", "", nil, ungranted, "", 403},
		{"offline", "UPDATE settings SET offline=true", "UPDATE settings SET offline=false", nil, key, "", 503},
		{"project raised", "UPDATE projects SET classification='restricted'", "UPDATE projects SET classification='internal'", nil, key, "", 403},
		{"revoked", "UPDATE api_keys SET revoked_at=now()", "UPDATE api_keys SET revoked_at=NULL", nil, key, "", 401},
		{"membership", "DELETE FROM role_bindings", "INSERT INTO role_bindings(id,role,subject_type,subject_id,project_id) VALUES($1,'member','user',$2,$3)", []any{ids.New(), user.ID, project}, key, "", 403},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if fixture.sql != "" {
				exec(t, pool, fixture.sql)
			}
			if fixture.undo != "" {
				defer exec(t, pool, fixture.undo, fixture.args...)
			}
			if status := call(fixture.token, fixture.raise); status != fixture.want || calls.Load() != 1 {
				t.Fatalf("denial status=%d calls=%d", status, calls.Load())
			}
		})
	}
	if status := call(key, "", "X-Starlight-Project", ids.New()); status != 403 || calls.Load() != 1 {
		t.Fatal("client project hint escaped fixed project")
	}
	exec(t, pool, "UPDATE projects SET secrets_guard='block'")
	original := body
	body = []byte(strings.Replace(string(body), "private-argument", key, 1))
	if status := call(key, ""); status != 403 || calls.Load() != 1 {
		t.Fatal("project secret screen bypassed")
	}
	body = original
	if status := call(key, "", "Mcp-Param-Opaque", "=?base64?"+base64.StdEncoding.EncodeToString([]byte(key))+"?="); status != 403 || calls.Load() != 1 {
		t.Fatal("encoded MCP header bypassed secret screen")
	}
	var inference, events int
	var details string
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM usage_events").Scan(&inference); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*),string_agg(details::text,' ') FROM audit_events WHERE action LIKE 'mcp.%'").Scan(&events, &details); err != nil {
		t.Fatal(err)
	}
	if inference != 0 || events != 2 || strings.Contains(details, "private-argument") || strings.Contains(details, "private-result") || strings.Contains(details, key) || strings.Contains(details, "fixture-upstream") {
		t.Fatal("tool accounting mixed inference or recorded content/credentials")
	}
	if _, err := audit.Verify(ctx, pool); err != nil {
		t.Fatal(err)
	}
}

func TestMCPRouteIsAbsentByDefault(t *testing.T) {
	f := startStarlight(t, nil)
	res, _ := f.call("POST", "/mcp", f.key, `{}`)
	if res.StatusCode != http.StatusNotFound {
		t.Fatal("default-off route exposed")
	}
}

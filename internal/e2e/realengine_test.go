//go:build !windows

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/control/wire"
)

// TestRealEngine runs built binaries against the engine in STARLIGHT_REAL_ENGINE and the model in STARLIGHT_REAL_MODEL.
func TestRealEngine(t *testing.T) {
	engine, model := os.Getenv("STARLIGHT_REAL_ENGINE"), os.Getenv("STARLIGHT_REAL_MODEL")
	if engine == "" || model == "" {
		t.Skip("set STARLIGHT_REAL_ENGINE to a TensorFold binary and STARLIGHT_REAL_MODEL to a checkpoint folder it serves")
	}
	version, err := osexec.Command(engine, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v\n%s", engine, err, version)
	}
	t.Logf("engine: %s", strings.TrimSpace(string(version)))
	d := newDrill(t)
	d.engine = engine
	if out, err := d.run(d.env(d.url, d.master, "127.0.0.1:1"), d.tool("starlight"), "init", "--org", "Example Corp"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	f := &fleet{t: t, ctx: context.Background(), pool: d.pool}
	f.catalogue()
	spec, _ := json.Marshal(wire.Spec{Model: wire.Artifact{Kind: "path", Path: model}, Parallel: 2})
	exec(t, d.pool, "UPDATE deployments SET spec = $1 WHERE name = 'qwen-mac'", spec)
	a := d.serve("first", d.url, d.master)
	d.waitFor("the instance to lead", 30*time.Second, func() bool { return a.epoch() > 0 })
	d.enrol(a)
	// A killed agent leaves its engine running on macOS, so stop the agent as a service manager would, first.
	t.Cleanup(func() {
		for _, p := range d.procs {
			if p.name == "node" {
				_ = p.cmd.Process.Signal(syscall.SIGTERM)
				select {
				case <-p.done:
				case <-time.After(30 * time.Second):
					t.Errorf("the node agent did not stop within 30 seconds of SIGTERM")
				}
			}
		}
	})
	loading := time.Now()
	d.waitFor("the replica to become ready", 10*time.Minute, func() bool { return d.ready("qwen-mac") })
	t.Logf("the replica was ready %s after enrolment", time.Since(loading).Round(time.Second))
	// The fleet helpers call Starlight at this URL: here, the instance's own public listener.
	f.public = &httptest.Server{URL: "http://" + a.public}

	resp, body := f.call("POST", "/v1/chat/completions", f.key, `{"model":"claude-sonnet-4-5","max_tokens":48,"messages":[{"role":"user","content":"Name three primary colours."}]}`)
	var chat struct {
		Choices []struct {
			Message struct {
				Content   string
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if resp.StatusCode != 200 || resp.Header.Get("X-Starlight-Route") != "local" || json.Unmarshal([]byte(body), &chat) != nil ||
		len(chat.Choices) != 1 || chat.Choices[0].Message.Content+chat.Choices[0].Message.Reasoning == "" || chat.Usage.CompletionTokens == 0 {
		t.Fatalf("chat %d %v %s", resp.StatusCode, resp.Header, body)
	}
	// A model that thinks first can spend a short reply's whole budget reasoning, so either part counts.
	t.Logf("chat replied %q, reasoning %q, in %d tokens", chat.Choices[0].Message.Content, chat.Choices[0].Message.Reasoning,
		chat.Usage.CompletionTokens)
	resp, body = f.call("POST", "/v1/chat/completions", f.key, `{"model":"qwen3.8-27b","stream":true,"max_tokens":32,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"Count to five."}]}`)
	if resp.StatusCode != 200 || !strings.Contains(body, "data: [DONE]") || !strings.Contains(body, `"usage"`) {
		t.Fatalf("stream %d %s", resp.StatusCode, body)
	}
	resp, body = f.call("POST", "/v1/completions", f.key, `{"model":"qwen3.8-27b","max_tokens":16,"prompt":"The capital of France is"}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"text"`) {
		t.Fatalf("completions %d %s", resp.StatusCode, body)
	}
	resp, body = f.call("POST", "/v1/messages", "", `{"model":"claude-sonnet-4-5","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"Say hello."}]}`,
		"X-Api-Key", f.key, "Anthropic-Version", "2023-06-01")
	if resp.StatusCode != 200 || !strings.Contains(body, "message_stop") {
		t.Fatalf("messages %d %s", resp.StatusCode, body)
	}
	resp, body = f.call("POST", "/v1/responses", f.key, `{"model":"qwen3.8-27b","max_output_tokens":32,"input":"Remember the number 7."}`)
	var created struct{ ID string }
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &created) != nil || created.ID == "" {
		t.Fatalf("responses %d %s", resp.StatusCode, body)
	}
	if resp, body := f.call("GET", "/v1/responses/"+created.ID, f.key, ""); resp.StatusCode != 200 || !strings.Contains(body, created.ID) {
		t.Fatalf("read back %d %s", resp.StatusCode, body)
	}
	if resp, body := f.call("DELETE", "/v1/responses/"+created.ID, f.key, ""); resp.StatusCode != 200 {
		t.Fatalf("delete %d %s", resp.StatusCode, body)
	}
	if resp, _ := f.call("GET", "/v1/responses/"+created.ID, f.key, ""); resp.StatusCode != 404 {
		t.Fatalf("a deleted response answered %d", resp.StatusCode)
	}
	resp, body = f.call("GET", "/v1/models", f.key, "")
	if resp.StatusCode != 200 || !strings.Contains(body, `"qwen3.8-27b"`) {
		t.Fatalf("models %d %s", resp.StatusCode, body)
	}
	if resp, _ := f.call("GET", "/v1/models", "slr_not_a_key", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a bad key answered %d", resp.StatusCode)
	}
	var rows, tokens int
	if err := d.pool.QueryRow(f.ctx, "SELECT count(*), coalesce(sum(prompt_tokens + completion_tokens), 0) FROM usage_events WHERE outcome = 'ok'").
		Scan(&rows, &tokens); err != nil {
		t.Fatal(err)
	}
	if rows < 5 || tokens == 0 {
		t.Fatalf("usage rows %d tokens %d", rows, tokens)
	}
	t.Logf("%d usage rows, %d tokens", rows, tokens)
}

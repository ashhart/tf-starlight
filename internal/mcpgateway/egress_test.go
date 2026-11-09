package mcpgateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPRedirectsNeverDispatchASecondRequest(t *testing.T) {
	for _, status := range []int{307, 308} {
		for _, otherHost := range []bool{false, true} {
			var sourceCalls, targetCalls atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1); complete(w, r) }))
			t.Cleanup(target.Close)
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sourceCalls.Add(1)
				location := "/follow"
				if otherHost {
					location = strings.Replace(target.URL, "127.0.0.1", "localhost", 1) + "/follow"
				}
				w.Header().Set("Location", location)
				w.WriteHeader(status)
			}))
			t.Cleanup(source.Close)
			u, err := newUpstream(source.URL, "fixture-credential", true, func() bool { return false })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(u.HTTP.CloseIdleConnections)
			h, raw := fixtureRequest("tools/call")
			x, err := u.send(context.Background(), h, raw)
			if err != nil || x.response.StatusCode != status || sourceCalls.Load() != 1 || targetCalls.Load() != 0 {
				t.Fatal("redirect retried or forwarded the tool credential")
			}
			x.close()
			u.HTTP.CloseIdleConnections()
			source.Close()
			target.Close()
		}
	}
}

func TestMCPPrivateHostnameResolutionIsRefusedByTheAddressGuard(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); complete(w, r) }))
	defer server.Close()
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	u, err := newUpstream("https://localhost:"+port, "fixture-credential", false, func() bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	defer u.HTTP.CloseIdleConnections()
	_, err = u.HTTP.Get(u.URL.String())
	if err == nil || !strings.Contains(err.Error(), "refusing internal address") || calls.Load() != 0 {
		t.Fatal("private hostname was not stopped by the egress address guard")
	}
}

func TestMCPProxyEnvironmentIsIgnoredInAFreshProcess(t *testing.T) {
	if os.Getenv("STARLIGHT_PROXY_FIXTURE_CHILD") == "1" {
		u, err := newUpstream("http://mcp-proxy-fixture.invalid/mcp", "fixture-credential", true, func() bool { return false })
		if err != nil {
			t.Fatal(err)
		}
		defer u.HTTP.CloseIdleConnections()
		u.Timeout = 2 * time.Second
		h, raw := fixtureRequest("tools/call")
		x, err := u.send(context.Background(), h, raw)
		x.close()
		if err == nil {
			t.Fatal("reserved invalid endpoint unexpectedly reached a proxy")
		}
		return
	}
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		complete(w, r)
	}))
	defer proxy.Close()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-test.run=^TestMCPProxyEnvironmentIsIgnoredInAFreshProcess$", "-test.count=1")
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		lower := strings.ToLower(name)
		if lower == "http_proxy" || lower == "https_proxy" || lower == "all_proxy" || lower == "no_proxy" || name == "STARLIGHT_PROXY_FIXTURE_CHILD" {
			continue
		}
		command.Env = append(command.Env, entry)
	}
	command.Env = append(command.Env, "STARLIGHT_PROXY_FIXTURE_CHILD=1", "HTTP_PROXY="+proxy.URL, "HTTPS_PROXY="+proxy.URL, "ALL_PROXY="+proxy.URL, "NO_PROXY=",
		"http_proxy="+proxy.URL, "https_proxy="+proxy.URL, "all_proxy="+proxy.URL, "no_proxy=")
	output, err := command.CombinedOutput()
	if len(output) > 2048 {
		output = output[len(output)-2048:]
	}
	if err != nil || calls.Load() != 0 {
		t.Fatalf("MCP proxy isolation fixture failed: %v, requests=%d, output=%s", err, calls.Load(), output)
	}
}

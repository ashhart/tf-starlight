package mcpgateway

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUpstreamTimeoutEndsWithoutRetry(t *testing.T) {
	h, calls, events := handlerFixture(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	h.options.Config.Timeout.Duration = time.Second
	start := time.Now()
	w := callHandler(h, "tools/call", nil)
	if w.Code != 502 || calls.Load() != 1 || time.Since(start) > 3*time.Second || (*events)[1].Outcome != "upstream_incomplete" {
		t.Fatal("deadline did not bound one ambiguous exchange")
	}
}

func TestSlowRequestBodyCannotHoldAConcurrencySlotPastItsDeadline(t *testing.T) {
	h, calls, _ := handlerFixture(t, http.HandlerFunc(complete))
	h.options.Config.Timeout.Duration = time.Second
	server := httptest.NewServer(h)
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	_, err = fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: fixture\r\nContent-Length: 100\r\nContent-Type: application/json\r\n\r\n{")
	if err != nil {
		t.Fatal(err)
	}
	res, _ := http.ReadResponse(bufio.NewReader(conn), nil)
	if res != nil {
		res.Body.Close()
		if res.StatusCode != 413 {
			t.Fatal("incomplete body accepted")
		}
	}
	if time.Since(start) > 3*time.Second || calls.Load() != 0 || len(h.slots) != 0 {
		t.Fatal("slow body retained authority or capacity")
	}
}

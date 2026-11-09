package e2e_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/testutil/tracecollector"
)

type traceReply struct {
	response *http.Response
	body     []byte
	err      error
}

func traceCall(ctx context.Context, f *fleet) traceReply {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.public.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3.8-27b","max_tokens":2,"messages":[{"role":"user","content":"delay fixture"}]}`))
	if err != nil {
		return traceReply{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.key)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return traceReply{err: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return traceReply{response: response, body: body, err: err}
}

func TestGatewayPhaseDelaysAreAttributedOnTheirOwnClock(t *testing.T) {
	for _, phase := range []string{"gateway.prepare", "gateway.admission", "gateway.settlement"} {
		t.Run(phase, func(t *testing.T) {
			collector := tracecollector.New(t)
			f := tracedFleet(t, collector, 1000000, "0")
			ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
			defer cancel()
			blocker, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			query := "LOCK TABLE models IN ACCESS EXCLUSIVE MODE"
			needle := "WITH target AS"
			switch phase {
			case "gateway.admission":
				query = "SELECT pg_advisory_xact_lock(hashtextextended('starlight:target:' || id::text, 0)) FROM replicas WHERE state='ready'"
				needle = "starlight_admit("
			case "gateway.settlement":
				query = "LOCK TABLE usage_events IN ACCESS EXCLUSIVE MODE"
				needle = "starlight_settle"
			}
			if _, err := blocker.Exec(ctx, query); err != nil {
				t.Fatal(err)
			}
			f.starlight.Catalog.Flush()
			done := make(chan traceReply, 1)
			go func() { done <- traceCall(ctx, f) }()
			deadline := time.Now().Add(4 * time.Second)
			for {
				var waiting int
				err := f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()
					AND pid<>pg_backend_pid() AND state='active' AND wait_event_type='Lock' AND query ILIKE '%'||$1||'%'`, needle).Scan(&waiting)
				if err != nil {
					t.Fatal(err)
				}
				if waiting > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("controlled delay did not reach the selected phase")
				}
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(40 * time.Millisecond)
			if err := blocker.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			var result traceReply
			select {
			case result = <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if result.err != nil || result.response.StatusCode != http.StatusOK {
				t.Fatalf("delayed request failed: %v %s", result.err, result.body)
			}
			id := strings.TrimPrefix(result.response.Header.Get("X-Request-Id"), "req_")
			frames := collector.Wait(t, id, "gateway.request", "gateway.prepare", "gateway.admission", "gateway.dispatch",
				"gateway.relay", "gateway.settlement")
			measured := frames[phase]
			if d := time.Duration(measured.End - measured.Start); d < 40*time.Millisecond {
				t.Fatalf("selected phase omitted its controlled lock wait: %s=%v", phase, d)
			}
			order := []string{"gateway.prepare", "gateway.admission", "gateway.dispatch", "gateway.relay", "gateway.settlement"}
			for i := 1; i < len(order); i++ {
				if frames[order[i-1]].End > frames[order[i]].Start {
					t.Fatal("same-host phase intervals overlap or subtract unrelated clocks")
				}
			}
		})
	}
}

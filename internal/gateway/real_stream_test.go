package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/httpx"
	"github.com/ashhart/tf-starlight/internal/providers"
)

type streamGateCase struct {
	name, path, protocol, body string
	tool                       bool
}

var streamGateCases = []streamGateCase{
	{"chat", "/v1/chat/completions", providers.ChatCompletions,
		`{"stream":true,"max_tokens":64,"messages":[{"role":"user","content":"Say hello in one short sentence."}]}`, false},
	{"chat_tool", "/v1/chat/completions", providers.ChatCompletions,
		`{"stream":true,"max_tokens":256,"messages":[{"role":"user","content":"Call get_weather for London. Do not answer in prose."}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get the weather for a city.","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],"tool_choice":{"type":"function","function":{"name":"get_weather"}}}`, true},
	{"responses_tool", "/v1/responses", providers.Responses,
		`{"stream":true,"max_output_tokens":256,"input":"Call get_weather for London. Do not answer in prose.","tools":[{"type":"function","name":"get_weather","description":"Get the weather for a city.","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"tool_choice":{"type":"function","name":"get_weather"}}`, true},
	{"messages_tool", "/v1/messages", providers.Messages,
		`{"stream":true,"max_tokens":256,"messages":[{"role":"user","content":"Call get_weather for London. Do not answer in prose."}],"tools":[{"name":"get_weather","description":"Get the weather for a city.","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"tool_choice":{"type":"tool","name":"get_weather"}}`, true},
}

type streamGateReceipt struct {
	GatewayOutputEvent int         `json:"gateway_output_event"`
	ClientOutputEvent  int         `json:"client_output_event"`
	ClientPayload      string      `json:"client_first_payload_kind"`
	Outcome            string      `json:"outcome"`
	Case               string      `json:"case"`
	EngineSHA          string      `json:"requested_engine_sha"`
	Bytes              int         `json:"body_bytes"`
	Digest             string      `json:"body_sha256"`
	Payload            payloadKind `json:"first_payload_kind"`
	Tool               bool        `json:"tool_payload_observed"`
	ClientHeaders      float64     `json:"client_headers_seconds"`
	ClientData         float64     `json:"client_first_body_read_seconds"`
	ClientOutput       float64     `json:"client_first_model_output_seconds"`
	ClientDone         float64     `json:"client_last_body_read_seconds"`
	GatewayStart       float64     `json:"gateway_start_after_client_send_seconds"`
	GatewayHeaders     float64     `json:"gateway_headers_seconds"`
	GatewayData        float64     `json:"gateway_first_body_read_seconds"`
	GatewayOutput      float64     `json:"gateway_first_model_output_seconds"`
	GatewayFlush       float64     `json:"gateway_model_flush_attempt_seconds"`
}

// TestRealEngineStreamTimings observes an already-running loopback server without starting or modifying an engine.
func TestRealEngineStreamTimings(t *testing.T) {
	base, model, head := os.Getenv("STARLIGHT_STREAM_GATE_URL"), os.Getenv("STARLIGHT_STREAM_GATE_MODEL"), os.Getenv("STARLIGHT_STREAM_ENGINE_SHA")
	if base == "" {
		t.Skip("set STARLIGHT_STREAM_GATE_URL, STARLIGHT_STREAM_GATE_MODEL and STARLIGHT_STREAM_ENGINE_SHA")
	}
	if err := validateStreamGateEndpoint(base); err != nil {
		t.Fatal(err)
	}
	if model == "" || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(head) {
		t.Fatal("declare the served model and full engine revision")
	}
	for _, c := range streamGateCases {
		t.Run(c.name, func(t *testing.T) {
			receipt, err := runStreamGate(base, model, os.Getenv("STARLIGHT_STREAM_GATE_KEY"), c)
			if err != nil {
				t.Fatal(err)
			}
			receipt.EngineSHA = head
			raw, _ := json.Marshal(receipt)
			t.Logf("stream_gate_receipt %s", raw)
		})
	}
}

func runStreamGate(base, model, key string, c streamGateCase, wrappers ...func(http.ResponseWriter) http.ResponseWriter) (streamGateReceipt, error) {
	receipt := streamGateReceipt{Case: c.name}
	if err := validateStreamGateEndpoint(base); err != nil {
		return receipt, err
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(c.body), &body); err != nil {
		return receipt, err
	}
	body["model"] = model
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	type result struct {
		start time.Time
		out   relayed
		wire  []byte
		err   error
	}
	results := make(chan result, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(base, "/")+c.path, bytes.NewReader(raw))
		if err != nil {
			results <- result{err: errors.New("invalid upstream request")}
			http.Error(w, "request failed", 502)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Anthropic-Version", "2023-06-01")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("X-Api-Key", key)
		}
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			results <- result{err: errors.New("upstream connection failed")}
			http.Error(w, "upstream failed", 502)
			return
		}
		defer resp.Body.Close()
		var captured bytes.Buffer
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.TeeReader(io.LimitReader(resp.Body, (2<<20)+1), &captured), resp.Body}
		dialect := httpx.OpenAI
		if c.protocol == providers.Messages {
			dialect = httpx.Anthropic
		}
		destination := http.ResponseWriter(w)
		for _, wrap := range wrappers {
			destination = wrap(destination)
		}
		out := relay(r.Context(), destination, resp, c.protocol, false, dialect, start, true)
		results <- result{start: start, out: out, wire: bytes.Clone(captured.Bytes())}
	}))
	defer func() { cancel(); proxy.Close() }()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, proxy.URL, nil)
	sent := time.Now()
	resp, err := proxy.Client().Do(req)
	if err != nil {
		return receipt, errors.New("client connection failed")
	}
	defer resp.Body.Close()
	receipt.ClientHeaders = time.Since(sent).Seconds()
	reader := bufio.NewReader(io.LimitReader(resp.Body, (2<<20)+1))
	var wire, block bytes.Buffer
	var outputAt time.Time
	complete := false
	events := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if wire.Len() == 0 {
				receipt.ClientData = time.Since(sent).Seconds()
			}
			wire.Write(line)
			block.Write(line)
		}
		if len(line) > 0 && len(bytes.TrimRight(line, "\r\n")) == 0 {
			events++
			data := clientGateData(block.Bytes())
			kind, tool := clientGatePayload(c.protocol, data)
			if kind != "" && outputAt.IsZero() {
				outputAt = time.Now()
				receipt.ClientOutput = outputAt.Sub(sent).Seconds()
				receipt.ClientOutputEvent = events
				receipt.ClientPayload = kind
			}
			if tool {
				receipt.Tool = true
			}
			var event struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(data, &event)
			complete = complete || (c.protocol == providers.ChatCompletions && bytes.Equal(data, []byte("[DONE]"))) ||
				(c.protocol == providers.Responses && event.Type == "response.completed") || (c.protocol == providers.Messages && event.Type == "message_stop")
			block.Reset()
		}
		if readErr != nil {
			if readErr != io.EOF {
				return receipt, errors.New("client body read failed")
			}
			break
		}
	}
	receipt.ClientDone = time.Since(sent).Seconds()
	var got result
	select {
	case got = <-results:
	case <-ctx.Done():
		return receipt, errors.New("relay did not finish")
	}
	if got.err != nil {
		return receipt, got.err
	}
	if resp.StatusCode != 200 || got.out.status != 200 || (got.out.outcome != "ok" && got.out.outcome != "ok_unmetered") {
		return receipt, fmt.Errorf("unsuccessful relay, status %d, outcome %s", resp.StatusCode, got.out.outcome)
	}
	if wire.Len() > 2<<20 || !bytes.Equal(wire.Bytes(), got.wire) {
		return receipt, errors.New("forwarded body bytes differ or exceed the fixture limit")
	}
	times := got.out.times
	if times == nil || !times.data.seen || !times.output.seen || !times.flush.seen || times.flushError || outputAt.IsZero() {
		return receipt, errors.New("missing model output or successful flush observation")
	}
	if got.out.firstByte > times.data.elapsed || times.data.elapsed > times.output.elapsed || times.output.elapsed > times.flush.elapsed ||
		outputAt.Before(got.start.Add(times.output.elapsed)) {
		return receipt, errors.New("gateway and client clock order is invalid")
	}
	if times.events != receipt.ClientOutputEvent || string(times.kind) != receipt.ClientPayload {
		return receipt, errors.New("gateway and independent client disagree on the first output event")
	}
	receipt.GatewayOutputEvent = times.events
	if !complete || c.tool && !receipt.Tool {
		return receipt, errors.New("missing terminal event or required tool payload")
	}
	digest := sha256.Sum256(wire.Bytes())
	receipt.Outcome = got.out.outcome
	receipt.Bytes, receipt.Digest, receipt.Payload = wire.Len(), hex.EncodeToString(digest[:]), times.kind
	receipt.GatewayStart, receipt.GatewayHeaders = got.start.Sub(sent).Seconds(), got.out.firstByte.Seconds()
	receipt.GatewayData, receipt.GatewayOutput, receipt.GatewayFlush = times.data.elapsed.Seconds(), times.output.elapsed.Seconds(), times.flush.elapsed.Seconds()
	return receipt, nil
}

func TestStreamGateFixtureChecksEachProtocol(t *testing.T) {
	for _, c := range streamGateCases {
		t.Run(c.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				payload := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{\"}}]}}]}\n\ndata: [DONE]\n\n"
				if !c.tool {
					payload = "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"
				}
				if c.protocol == providers.Responses {
					payload = "data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"
				}
				if c.protocol == providers.Messages {
					payload = "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				_, _ = io.WriteString(w, ": heartbeat\n\n")
				if c.protocol == providers.ChatCompletions {
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
				}
				_ = http.NewResponseController(w).Flush()
				_, _ = io.WriteString(w, payload)
			}))
			defer upstream.Close()
			if _, err := runStreamGate(upstream.URL, "fixture", "", c); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStreamGateRejectsMetadataOnlyAndIncompleteStreams(t *testing.T) {
	for _, body := range []string{
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: [DONE]\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
	} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, body)
		}))
		_, err := runStreamGate(upstream.URL, "fixture", "", streamGateCases[0])
		upstream.Close()
		if err == nil {
			t.Fatal("unqualified stream passed the gate")
		}
	}
}

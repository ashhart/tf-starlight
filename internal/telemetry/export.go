package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const responseLimit = 64 << 10

type exportResult struct {
	class      string
	retry      bool
	retryAfter time.Duration
	rejected   int
	partial    bool
}

func (r *Recorder) export(batch []record) string {
	data, err := marshalBatch(r.options.Component, batch)
	if err != nil {
		r.stats.invalid.Add(uint64(len(batch)))
		return "encoding"
	}
	var result exportResult
	for attempt := 0; attempt < 2; attempt++ {
		if r.ctx.Err() != nil {
			r.stats.shutdown.Add(uint64(len(batch)))
			return ""
		}
		if !r.permitted() {
			if r.ctx.Err() != nil {
				r.stats.shutdown.Add(uint64(len(batch)))
				return ""
			}
			r.stats.policy.Add(uint64(len(batch)))
			return "policy"
		}
		result = r.send(data, len(batch))
		if result.class == "" {
			r.stats.sent.Add(uint64(len(batch) - result.rejected))
			r.stats.rejected.Add(uint64(result.rejected))
			if result.partial {
				r.stats.partial.Add(1)
			}
			if result.rejected < len(batch) {
				r.stats.lastSuccess.Store(time.Now().Unix())
			}
			if result.rejected > 0 {
				return "partial_rejection"
			}
			return ""
		}
		if !result.retry || attempt == 1 || result.retryAfter > r.options.Timeout {
			break
		}
		delay := max(100*time.Millisecond, result.retryAfter)
		timer := time.NewTimer(delay)
		select {
		case <-r.ctx.Done():
			timer.Stop()
		case <-timer.C:
			r.stats.retries.Add(1)
		}
	}
	if r.ctx.Err() != nil {
		r.stats.shutdown.Add(uint64(len(batch)))
		return ""
	}
	r.stats.failed.Add(uint64(len(batch)))
	return result.class
}

func (r *Recorder) send(data []byte, count int) exportResult {
	ctx, cancel := context.WithTimeout(r.ctx, r.options.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.options.Endpoint, bytes.NewReader(data))
	if err != nil {
		return exportResult{class: "request"}
	}
	req.Header.Set("Content-Type", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	r.stats.requests.Add(1)
	response, err := r.client.Do(req)
	if err != nil {
		return exportResult{class: "transport", retry: true}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		retry := response.StatusCode == 429 || response.StatusCode == 502 || response.StatusCode == 503 || response.StatusCode == 504
		return exportResult{class: "http_status", retry: retry, retryAfter: retryDelay(response.Header.Get("Retry-After"))}
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || response.Header.Get("Content-Encoding") != "" {
		return exportResult{class: "response_encoding"}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if err != nil || len(body) > responseLimit {
		return exportResult{class: "response_body"}
	}
	return parseResponse(body, count)
}

func parseResponse(body []byte, count int) exportResult {
	body = bytes.TrimSpace(body)
	var reply struct {
		Partial *struct {
			Rejected json.RawMessage `json:"rejectedSpans"`
		} `json:"partialSuccess"`
	}
	if len(body) == 0 || body[0] != '{' || json.Unmarshal(body, &reply) != nil {
		return exportResult{class: "response_json"}
	}
	if reply.Partial == nil {
		return exportResult{}
	}
	n := strings.TrimSpace(string(reply.Partial.Rejected))
	if strings.HasPrefix(n, "\"") {
		if json.Unmarshal(reply.Partial.Rejected, &n) != nil {
			return exportResult{class: "response_rejection"}
		}
	}
	if n == "" || n == "null" {
		n = "0"
	}
	rejected, err := strconv.Atoi(n)
	if err != nil || rejected < 0 || rejected > count {
		return exportResult{class: "response_rejection"}
	}
	return exportResult{partial: true, rejected: rejected}
}

func retryDelay(header string) time.Duration {
	if header == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(header, 10, 32); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(header); err == nil {
		return max(0, time.Until(at))
	}
	return 0
}

func (r *Recorder) permitted() bool {
	if r.options.Allowed == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(r.ctx, r.options.Timeout)
	defer cancel()
	return r.options.Allowed(ctx) && ctx.Err() == nil
}

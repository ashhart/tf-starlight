package gateway

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/httpx"
)

const (
	maxEvent    = 16 << 20
	maxResponse = 64 << 20
)

// relayed is what the client received and what it cost.
type relayed struct {
	status     int
	usage      admission.Usage
	firstByte  time.Duration
	outcome    string
	responseID string
	times      *streamTimes
}

// relay copies an upstream response to the client and meters usage; stripUsage hides an unrequested usage chunk.
func relay(ctx context.Context, w http.ResponseWriter, resp *http.Response, protocol string, stripUsage bool, d httpx.Dialect, start time.Time, observe bool) relayed {
	return relayWithCalibration(ctx, w, resp, protocol, stripUsage, d, start, observe, false)
}

func relayWithCalibration(ctx context.Context, w http.ResponseWriter, resp *http.Response, protocol string, stripUsage bool, d httpx.Dialect, start time.Time, observe, calibrateTokens bool) relayed {
	out := relayed{status: resp.StatusCode, outcome: "ok"}
	m := &meter{protocol: protocol, calibrateTokens: calibrateTokens}
	ct := resp.Header.Get("Content-Type")
	w.Header().Set("Content-Type", ct)
	if strings.HasPrefix(ct, "text/event-stream") && resp.StatusCode == http.StatusOK {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(resp.StatusCode)
		out.firstByte = time.Since(start)
		if observe {
			out.times = &streamTimes{start: start}
		}
		out.outcome = stream(ctx, w, resp.Body, m, stripUsage, d, out.times)
	} else {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
		if err != nil || len(body) > maxResponse {
			out.outcome, out.status = "interrupted", http.StatusBadGateway
			httpx.Write(w, d, httpx.Errorf(http.StatusBadGateway, "upstream_error", "the model server's response could not be read"))
			return out
		}
		out.firstByte = time.Since(start)
		if resp.StatusCode < 300 {
			m.body(body)
		} else {
			out.outcome = outcomeFor(resp.StatusCode)
			m.usage.Observed = true
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	}
	out.usage, out.responseID = m.usage, m.responseID
	return out
}

func outcomeFor(status int) string {
	switch {
	case status >= 500:
		return "upstream_error"
	case status >= 400:
		return "client_error"
	}
	return "ok"
}

// stream forwards server-sent events one block at a time, flushing each to the client.
func stream(ctx context.Context, w http.ResponseWriter, body io.Reader, m *meter, stripUsage bool, d httpx.Dialect, times *streamTimes) string {
	rc := http.NewResponseController(w)
	if times != nil {
		body = observedReader{Reader: body, times: times}
	}
	r := bufio.NewReaderSize(body, 64<<10)
	var block bytes.Buffer
	for {
		line, err := readLine(r)
		block.Write(line)
		if block.Len() > maxEvent {
			return "interrupted"
		}
		blank := len(line) > 0 && len(bytes.TrimRight(line, "\r\n")) == 0
		if blank || err != nil && block.Len() > 0 {
			firstOutput := blank && times.event(m.protocol, block.Bytes())
			if !forward(w, block.Bytes(), m, stripUsage) {
				return "cancelled"
			}
			if firstOutput {
				times.flush = seenTime{elapsed: times.elapsed(), seen: true}
			}
			flushErr := rc.Flush()
			if firstOutput {
				times.flushError = flushErr != nil
			}
			block.Reset()
		}
		if err != nil {
			return ended(ctx, err, m, w, d, rc)
		}
	}
}

// ended classifies how a stream finished; a stream that ends without usage is charged its reservation.
func ended(ctx context.Context, err error, m *meter, w http.ResponseWriter, d httpx.Dialect, rc *http.ResponseController) string {
	switch {
	case errors.Is(err, io.EOF) && m.usage.Observed:
		return "ok"
	case ctx.Err() != nil:
		return "cancelled"
	case errors.Is(err, io.EOF):
		return "ok_unmetered"
	}
	writeStreamError(w, d)
	_ = rc.Flush()
	return "interrupted"
}

func readLine(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.ReadSlice('\n')
		out = append(out, chunk...)
		if err != bufio.ErrBufferFull {
			return out, err
		}
		if len(out) > maxEvent {
			return out, errors.New("event too large")
		}
	}
}

// forward meters one event block and writes it unless it is a hidden usage chunk; false means the client left.
func forward(w http.ResponseWriter, block []byte, m *meter, stripUsage bool) bool {
	hide := false
	for _, line := range bytes.Split(block, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if m.event(data) && stripUsage {
			hide = true
		}
	}
	if hide {
		return true
	}
	_, err := w.Write(block)
	return err == nil
}

func writeStreamError(w http.ResponseWriter, d httpx.Dialect) {
	if d == httpx.Anthropic {
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"the model stream was interrupted\"}}\n\n")
		return
	}
	_, _ = io.WriteString(w, "data: {\"error\":{\"message\":\"the model stream was interrupted\",\"type\":\"server_error\",\"code\":\"stream_interrupted\"}}\n\ndata: [DONE]\n\n")
}

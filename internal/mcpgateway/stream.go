package mcpgateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

type eventReader struct {
	reader    *bufio.Reader
	remaining int64
	maxFrame  int
}

func (r *eventReader) next() ([]byte, error) {
	var block []byte
	for {
		line, err := r.reader.ReadSlice('\n')
		r.remaining -= int64(len(line))
		if r.remaining < 0 || len(block)+len(line) > r.maxFrame {
			return nil, errors.New("MCP event stream exceeds its bounds")
		}
		block = append(block, line...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF && len(block) == 0 {
				return nil, io.EOF
			}
			return nil, errors.New("MCP event stream is incomplete")
		}
		if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
			return block, nil
		}
	}
}

func rpcEvent(block []byte) []byte {
	var fields [][]byte
	for _, line := range bytes.Split(block, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		fields = append(fields, bytes.TrimPrefix(line[5:], []byte(" ")))
	}
	if len(fields) == 0 {
		return nil
	}
	return bytes.Join(fields, []byte("\n"))
}

// relayEvents forwards validated request-scoped messages and stops after one final response.
func relayEvents(ctx context.Context, w http.ResponseWriter, input io.Reader, expected request, allowed map[string]bool, maxBytes int64, maxFrame int) error {
	r := eventReader{reader: bufio.NewReaderSize(input, 32<<10), remaining: maxBytes, maxFrame: maxFrame}
	started := false
	lastProgress := -1.0
	for {
		block, err := r.next()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("MCP upstream ended before a final response")
		}
		data := rpcEvent(block)
		var wire []byte
		final := false
		if data == nil {
			wire = []byte(": keepalive\n\n")
		} else {
			data, final, err = response(data, expected, allowed, maxFrame)
			if err != nil {
				return err
			}
			if !final {
				var update struct {
					Params struct {
						Progress float64 `json:"progress"`
					} `json:"params"`
				}
				_ = json.Unmarshal(data, &update)
				if update.Params.Progress <= lastProgress {
					return errors.New("MCP progress did not increase")
				}
				lastProgress = update.Params.Progress
			}
			wire = append([]byte("event: message\ndata: "), data...)
			wire = append(wire, '\n', '\n')
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			started = true
		}
		if _, err := w.Write(wire); err != nil {
			return errors.New("MCP client write failed")
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			return errors.New("MCP client flush failed")
		}
		if final {
			return nil
		}
	}
}

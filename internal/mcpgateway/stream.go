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
	skipLF    bool
	began     bool
}

func (r *eventReader) next() ([]byte, error) {
	var block []byte
	frame := 0
	for {
		line, err := r.line(&frame)
		if err != nil {
			if err == io.EOF && frame == 0 {
				return nil, io.EOF
			}
			return nil, errors.New("MCP event stream is incomplete")
		}
		if !r.began {
			r.began = true
			line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
		}
		block = append(block, line...)
		block = append(block, '\n')
		if len(line) == 0 {
			return block, nil
		}
	}
}

func (r *eventReader) line(frame *int) ([]byte, error) {
	var line []byte
	for {
		ch, err := r.reader.ReadByte()
		if err != nil {
			return nil, err
		}
		r.remaining--
		*frame++
		if r.remaining < 0 || *frame > r.maxFrame {
			return nil, errors.New("MCP event stream exceeds its bounds")
		}
		if r.skipLF {
			r.skipLF = false
			if ch == '\n' {
				continue
			}
		}
		if ch == '\r' {
			r.skipLF = true
			return line, nil
		}
		if ch == '\n' {
			return line, nil
		}
		line = append(line, ch)
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
func relayEvents(ctx context.Context, w http.ResponseWriter, input io.Reader, expected request, allowed map[string]bool, maxBytes int64, maxFrame int, credential string) (bool, error) {
	r := eventReader{reader: bufio.NewReaderSize(input, 32<<10), remaining: maxBytes, maxFrame: maxFrame}
	started := false
	lastProgress := -1.0
	remaining, frames := maxBytes, 0
	for {
		frames++
		if frames > 4096 {
			return started, errors.New("MCP stream exceeds its frame count")
		}
		block, err := r.next()
		if err != nil {
			if ctx.Err() != nil {
				return started, ctx.Err()
			}
			return started, errors.New("MCP upstream ended before a final response")
		}
		data := rpcEvent(block)
		var wire []byte
		final := false
		if data == nil {
			wire = []byte(": keepalive\n\n")
		} else {
			data, final, err = response(data, expected, allowed, maxFrame, credential)
			if err != nil {
				return started, err
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, data); err != nil {
				return started, errors.New("MCP stream data is invalid")
			}
			data = compact.Bytes()
			if !final {
				var update struct {
					Params struct {
						Progress float64 `json:"progress"`
					} `json:"params"`
				}
				_ = json.Unmarshal(data, &update)
				if update.Params.Progress <= lastProgress {
					return started, errors.New("MCP progress did not increase")
				}
				lastProgress = update.Params.Progress
			}
			wire = append([]byte("event: message\ndata: "), data...)
			wire = append(wire, '\n', '\n')
		}
		remaining -= int64(len(wire))
		if remaining < 0 || len(wire) > maxFrame {
			return started, errors.New("MCP output stream exceeds its bounds")
		}
		if ctx.Err() != nil {
			return started, ctx.Err()
		}
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			started = true
		}
		if _, err := w.Write(wire); err != nil {
			return started, errors.New("MCP client write failed")
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			return started, errors.New("MCP client flush failed")
		}
		if final {
			return started, nil
		}
	}
}

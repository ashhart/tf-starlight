package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Recorder owns one fixed queue and worker; inference only attempts a nonblocking enqueue of a fixed-shape record.
type Recorder struct {
	options Options
	queue   chan record
	client  *http.Client
	token   string
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.RWMutex
	closed  bool
	stats   counters
}

// New starts no listener and returns nil when disabled; it never contacts a collector during construction.
func New(ctx context.Context, options Options) (*Recorder, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if options.Endpoint == "" {
		return nil, nil
	}
	o := options.defaults()
	token := ""
	if o.BearerTokenFile != "" {
		f, err := os.Open(o.BearerTokenFile)
		if err != nil {
			return nil, errors.New("could not open telemetry bearer token file")
		}
		b, err := io.ReadAll(io.LimitReader(f, 8193))
		f.Close()
		token = strings.TrimSpace(string(b))
		if err != nil || len(b) > 8192 || token == "" || strings.ContainsAny(token, "\r\n") {
			return nil, errors.New("telemetry bearer token file is empty, invalid or exceeds 8 KiB")
		}
	}
	t := &http.Transport{Proxy: nil, DisableCompression: true, MaxConnsPerHost: 1, MaxIdleConns: 1, MaxIdleConnsPerHost: 1,
		IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: o.Timeout, ResponseHeaderTimeout: o.Timeout,
		MaxResponseHeaderBytes: responseLimit,
		DialContext:            (&net.Dialer{Timeout: o.Timeout}).DialContext}
	r := &Recorder{options: o, queue: make(chan record, o.Queue), token: token, done: make(chan struct{})}
	r.ctx, r.cancel = context.WithCancel(ctx)
	r.client = &http.Client{Transport: t, Timeout: o.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	go r.run()
	return r, nil
}

func (r *Recorder) offer(v record) {
	if !r.mu.TryRLock() {
		r.stats.shutdown.Add(1)
		return
	}
	defer r.mu.RUnlock()
	if r.closed {
		r.stats.shutdown.Add(1)
		return
	}
	select {
	case r.queue <- v:
		r.stats.enqueued.Add(1)
	default:
		r.stats.queueFull.Add(1)
	}
}

func (r *Recorder) seal() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
}

// Close stops export and bounds the wait; pending telemetry is counted as shutdown loss, never held as audit data.
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.seal()
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(r.options.Timeout + time.Second):
	}
	r.client.CloseIdleConnections()
}

func (r *Recorder) run() {
	defer close(r.done)
	defer r.client.CloseIdleConnections()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	batch := make([]record, 0, r.options.Batch)
	var lastWarning time.Time
	flush := func() {
		if len(batch) == 0 {
			return
		}
		class := r.export(batch)
		if class != "" && time.Since(lastWarning) >= 15*time.Second {
			log := r.options.Logger
			if log == nil {
				log = slog.Default()
			}
			log.Warn("phase telemetry export failed", "class", class, "spans", len(batch))
			lastWarning = time.Now()
		}
		clear(batch)
		batch = batch[:0]
	}
	for {
		select {
		case <-r.ctx.Done():
			r.seal()
			r.stats.shutdown.Add(uint64(len(batch)))
			for len(r.queue) > 0 {
				<-r.queue
				r.stats.shutdown.Add(1)
			}
			return
		case v := <-r.queue:
			batch = append(batch, v)
			if len(batch) == r.options.Batch {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

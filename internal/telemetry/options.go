package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"
)

// Component admits only fixed service names, without hostname, installation, target or caller attributes.
type Component uint8

const (
	GatewayComponent Component = iota
	NodeComponent
)

// Options configure one explicit collector; an empty endpoint disables production and all exporter work.
type Options struct {
	Endpoint         string
	BearerTokenFile  string
	SamplePerMillion int
	Queue            int
	Batch            int
	Timeout          time.Duration
	Component        Component
	Logger           *slog.Logger
	Allowed          func(context.Context) bool
}

func (o Options) defaults() Options {
	if o.Queue == 0 {
		o.Queue = 256
	}
	if o.Batch == 0 {
		o.Batch = min(32, o.Queue)
	}
	if o.Timeout == 0 {
		o.Timeout = time.Second
	}
	return o
}

// Validate rejects unbounded settings and credential-bearing URLs, without echoing collector configuration.
func (o Options) Validate() error {
	o = o.defaults()
	if o.SamplePerMillion < 0 || o.SamplePerMillion > 1_000_000 || o.Queue < 1 || o.Queue > 4096 ||
		o.Batch < 1 || o.Batch > min(128, o.Queue) || o.Timeout < 10*time.Millisecond || o.Timeout > 5*time.Second ||
		o.Component > NodeComponent {
		return errors.New("telemetry settings exceed the sampling, queue, batch or timeout bounds")
	}
	if o.Endpoint == "" {
		if o.BearerTokenFile != "" {
			return errors.New("telemetry bearer token needs a collector endpoint")
		}
		return nil
	}
	u, err := url.Parse(o.Endpoint)
	if err != nil || len(o.Endpoint) > 2048 || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return errors.New("telemetry endpoint must be an absolute HTTP(S) URL without credentials, query or fragment")
	}
	if o.BearerTokenFile != "" && u.Scheme != "https" {
		if !LoopbackEndpoint(o.Endpoint) {
			return errors.New("telemetry bearer authentication requires HTTPS outside loopback")
		}
	}
	return nil
}

// LoopbackEndpoint admits literal loopback addresses or localhost without trusting DNS aliases.
func LoopbackEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return strings.EqualFold(u.Hostname(), "localhost") || ip != nil && ip.IsLoopback()
}

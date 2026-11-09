package server

import (
	"context"

	"github.com/ashhart/tf-starlight/internal/telemetry"
)

func (t *Starlight) buildTelemetry() error {
	if !t.Config.HasRole("gateway") {
		return nil
	}
	opts := t.Config.Telemetry.Options()
	opts.Logger = t.Log
	if opts.Endpoint != "" && !telemetry.LoopbackEndpoint(opts.Endpoint) {
		opts.Allowed = func(ctx context.Context) bool { return !t.Policies.Offline(ctx) }
	}
	r, err := telemetry.New(context.Background(), opts)
	if err != nil {
		return err
	}
	t.traces = r
	t.Gateway.SetTelemetry(r)
	if r != nil {
		t.Metrics.RegisterTelemetry(r)
	}
	return nil
}

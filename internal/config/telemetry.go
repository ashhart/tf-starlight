package config

import "github.com/ashhart/tf-starlight/internal/telemetry"

// Telemetry configures an explicitly enabled collector; payload fields and phase names are not configurable.
type Telemetry struct {
	Endpoint         string   `yaml:"endpoint"`
	BearerTokenFile  string   `yaml:"bearer_token_file"`
	SamplePerMillion int      `yaml:"sample_per_million"`
	Queue            int      `yaml:"queue"`
	Batch            int      `yaml:"batch"`
	Timeout          Duration `yaml:"timeout"`
}

// Options fixes the gateway component without accepting resource labels from configuration.
func (t Telemetry) Options() telemetry.Options {
	return telemetry.Options{Endpoint: t.Endpoint, BearerTokenFile: t.BearerTokenFile, SamplePerMillion: t.SamplePerMillion,
		Queue: t.Queue, Batch: t.Batch, Timeout: t.Timeout.Duration, Component: telemetry.GatewayComponent}
}

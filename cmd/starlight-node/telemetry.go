package main

import (
	"flag"
	"os"
	"time"

	"github.com/ashhart/tf-starlight/internal/telemetry"
)

func telemetryFlags(fs *flag.FlagSet, o *telemetry.Options) {
	fs.StringVar(&o.Endpoint, "telemetry-endpoint", os.Getenv("STARLIGHT_NODE_TELEMETRY_ENDPOINT"), "optional OTLP/HTTP JSON collector URL")
	fs.StringVar(&o.BearerTokenFile, "telemetry-bearer-token-file", os.Getenv("STARLIGHT_NODE_TELEMETRY_BEARER_TOKEN_FILE"), "collector bearer token file")
	fs.IntVar(&o.SamplePerMillion, "telemetry-sample-per-million", 10000, "phase traces sampled per million, 0..1000000")
	fs.IntVar(&o.Queue, "telemetry-queue", 256, "fixed span queue capacity, 1..4096")
	fs.IntVar(&o.Batch, "telemetry-batch", 32, "spans per export, at most 128 and queue capacity")
	fs.DurationVar(&o.Timeout, "telemetry-timeout", time.Second, "collector attempt deadline, 10ms..5s")
}

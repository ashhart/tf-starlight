package telemetry

import "sync/atomic"

type counters struct {
	policy                                                         atomic.Uint64
	enqueued, sent, queueFull, failed, rejected, shutdown, invalid atomic.Uint64
	requests, retries, partial                                     atomic.Uint64
	lastSuccess                                                    atomic.Int64
}

// Stats reports fixed exporter health counters; collector responses and identifiers never become labels.
type Stats struct {
	Policy                                                         uint64
	Enqueued, Sent, QueueFull, Failed, Rejected, Shutdown, Invalid uint64
	Requests, Retries, Partial                                     uint64
	QueueLength                                                    int
	LastSuccess                                                    int64
}

// Stats is safe on a disabled recorder and does not perform network or database work.
func (r *Recorder) Stats() Stats {
	if r == nil {
		return Stats{}
	}
	s := &r.stats
	return Stats{Enqueued: s.enqueued.Load(), Sent: s.sent.Load(), QueueFull: s.queueFull.Load(), Failed: s.failed.Load(),
		Rejected: s.rejected.Load(), Shutdown: s.shutdown.Load(), Invalid: s.invalid.Load(), Requests: s.requests.Load(),
		Retries: s.retries.Load(), Partial: s.partial.Load(), Policy: s.policy.Load(), QueueLength: len(r.queue), LastSuccess: s.lastSuccess.Load()}
}

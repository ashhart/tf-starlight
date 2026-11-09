package metrics

import (
	"context"
	"encoding/json"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ashhart/tf-starlight/internal/db"
	"github.com/ashhart/tf-starlight/internal/observations"
)

// ReplicaObservationLimit caps each scrape's replica label sets; stopped replicas leave on the next scrape.
const ReplicaObservationLimit = 1024

type replicaObservations struct {
	q                   db.Querier
	up, truncated       *prometheus.Desc
	age, present, valid *prometheus.Desc
	values              [5]*prometheus.Desc
}

// RegisterReplicaObservations adds a bounded scrape-time collector with no retained replica label cache.
func (s *Set) RegisterReplicaObservations(q db.Querier) {
	labels := []string{"model", "deployment", "replica"}
	c := &replicaObservations{q: q,
		up:        prometheus.NewDesc("starlight_replica_observation_scrape_success", "Whether the replica observation query completed.", nil, nil),
		truncated: prometheus.NewDesc("starlight_replica_observation_truncated", "Whether eligible replicas exceeded the scrape limit.", nil, nil),
		age:       prometheus.NewDesc("starlight_replica_observation_age_seconds", "Age of the node measurement; omitted for missing timestamps.", labels, nil),
		present:   prometheus.NewDesc("starlight_replica_observation_fresh", "Whether a timestamped node observation is current.", labels, nil),
		valid:     prometheus.NewDesc("starlight_replica_observation_valid", "Whether the named measurement is present, valid and current.", append(append([]string(nil), labels...), "measurement"), nil)}
	names := []string{"decode_tokens_per_second", "prompt_tokens_per_second", "running_requests", "waiting_requests", "kv_usage_ratio"}
	for i, name := range names {
		c.values[i] = prometheus.NewDesc("starlight_replica_"+name, "Current node-measured "+name+"; absent when unknown or expired.", labels, nil)
	}
	s.Registry.MustRegister(c)
}

func (c *replicaObservations) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.up, c.truncated, c.age, c.present, c.valid} {
		ch <- d
	}
	for _, d := range c.values {
		ch <- d
	}
}

type replicaObservation struct {
	model, deployment, replica string
	load                       observations.Load
	currentProcess             bool
}

func (c *replicaObservations) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := c.q.Query(ctx, `SELECT m.name, d.name, r.id, r.live, coalesce((r.live->>'generation')::bigint=r.generation,false) AND (r.ready_at IS NULL OR r.live_at>=r.ready_at) FROM replicas r
		JOIN deployments d ON d.id = r.deployment_id JOIN models m ON m.id = d.model_id JOIN nodes n ON n.id = r.node_id
		WHERE r.desired = 'running' AND r.state = 'ready' AND d.status = 'active' AND n.status = 'active'
		AND n.last_seen_at > now() - interval '90 seconds' ORDER BY r.id LIMIT $1`, ReplicaObservationLimit+1)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
		return
	}
	defer rows.Close()
	var samples []replicaObservation
	for rows.Next() {
		var s replicaObservation
		var raw []byte
		if err = rows.Scan(&s.model, &s.deployment, &s.replica, &raw, &s.currentProcess); err != nil {
			break
		}
		if json.Unmarshal(raw, &s.load) != nil {
			s.load = observations.Load{}
		}
		samples = append(samples, s)
	}
	if err != nil || rows.Err() != nil {
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1)
	overflow := 0.0
	if len(samples) > ReplicaObservationLimit {
		overflow = 1
		samples = samples[:ReplicaObservationLimit]
	}
	ch <- prometheus.MustNewConstMetric(c.truncated, prometheus.GaugeValue, overflow)
	now := time.Now()
	for _, s := range samples {
		c.emit(ch, s, now)
	}
}

func (c *replicaObservations) emit(ch chan<- prometheus.Metric, s replicaObservation, now time.Time) {
	labels := []string{s.model, s.deployment, s.replica}
	load, ok := s.load.Normalize(now)
	fresh := ok && load.Fresh(now) && s.currentProcess
	freshValue := 0.0
	if fresh {
		freshValue = 1
	}
	ch <- prometheus.MustNewConstMetric(c.present, prometheus.GaugeValue, freshValue, labels...)
	if !s.load.ObservedAt.IsZero() && !s.load.ObservedAt.After(now) {
		ch <- prometheus.MustNewConstMetric(c.age, prometheus.GaugeValue, now.Sub(s.load.ObservedAt).Seconds(), labels...)
	}
	var running, waiting *float64
	if load.Running != nil {
		v := float64(*load.Running)
		running = &v
	}
	if load.Waiting != nil {
		v := float64(*load.Waiting)
		waiting = &v
	}
	values := []*float64{load.TokensPerSecond, load.PromptTokensPerSecond, running, waiting, load.KVUsage}
	names := []string{"decode", "prompt", "running", "waiting", "kv"}
	for i, v := range values {
		valid := 0.0
		if fresh && v != nil {
			valid = 1
			ch <- prometheus.MustNewConstMetric(c.values[i], prometheus.GaugeValue, *v, labels...)
		}
		ch <- prometheus.MustNewConstMetric(c.valid, prometheus.GaugeValue, valid, append(append([]string(nil), labels...), names[i])...)
	}
}

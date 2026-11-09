package engine

import (
	"bufio"
	"context"
	"errors"
	"io"
	"maps"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ashhart/tf-starlight/internal/control/wire"
	"github.com/ashhart/tf-starlight/internal/observations"
)

// Engine metric families the node reads for live stats.
const (
	famGenTotal   = "tensorfold:generation_tokens_total"
	famGenRunning = "tensorfold:generation_tokens_running"
	famPrompt     = "tensorfold:prompt_tokens_total"
	famRunning    = "tensorfold:requests_running"
	famWaiting    = "tensorfold:requests_waiting"
	famKV         = "tensorfold:kv_cache_usage_ratio"
)

// sample is one scrape of an engine process: every value of each family, across its label sets.
type sample struct {
	at    time.Time
	base  string
	epoch uint64
	shape map[string]string
	fam   map[string][]float64
}

func (s *sample) sum(name string) (float64, bool) {
	vs, ok := s.fam[name]
	total := 0.0
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return 0, false
		}
		total += v
	}
	return total, ok && len(vs) > 0 && !math.IsInf(total, 0)
}

func (s *sample) max(name string) (float64, bool) {
	vs, ok := s.fam[name]
	m := 0.0
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return 0, false
		}
		m = max(m, v)
	}
	return m, ok && len(vs) > 0
}

// produced counts finished and still-streaming tokens, so long replies show throughput while they run.
func produced(s *sample) (float64, bool) {
	total, ok := counter(s, famGenTotal)
	if !ok {
		return 0, false
	}
	running, ok := counter(s, famGenRunning)
	if !ok {
		return 0, false
	}
	total += running
	return total, total <= observations.MaxCounter
}

func counter(s *sample, family string) (float64, bool) {
	v, ok := s.sum(family)
	for _, value := range s.fam[family] {
		if value > observations.MaxCounter || math.Trunc(value) != value {
			return 0, false
		}
	}
	return v, ok && v <= observations.MaxCounter
}

func prompted(s *sample) (float64, bool) { return counter(s, famPrompt) }

// measuredRate returns unknown for missing data, process changes, counter resets or invalid windows.
func measuredRate(prev, cur *sample, value func(*sample) (float64, bool)) *float64 {
	if prev == nil || cur == nil || prev.base != cur.base || prev.epoch != cur.epoch || !maps.Equal(prev.shape, cur.shape) {
		return nil
	}
	a, okA := value(prev)
	b, okB := value(cur)
	dt := cur.at.Sub(prev.at).Seconds()
	if !okA || !okB || b < a || dt <= 0 || dt > observations.MaxAge.Seconds() {
		return nil
	}
	v := (b - a) / dt
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

func measuredCount(s *sample, family string) *int {
	v, ok := s.sum(family)
	if !ok || v > math.MaxInt32 || math.Trunc(v) != v {
		return nil
	}
	n := int(v)
	return &n
}

// parseSample reads the Prometheus text exposition, keeping only the families the node uses.
func parseSample(r io.Reader) map[string][]float64 { return parseMetrics(r).fam }

func parseMetrics(r io.Reader) *sample {
	want := map[string]bool{famGenTotal: true, famGenRunning: true, famPrompt: true, famRunning: true, famWaiting: true, famKV: true}
	out := &sample{fam: map[string][]float64{}, shape: map[string]string{}}
	labels := map[string][]string{}
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		end := strings.IndexAny(line, "{ \t")
		if end <= 0 || !want[line[:end]] {
			continue
		}
		name, rest, key := line[:end], line[end:], ""
		value := math.NaN()
		if rest[0] == '{' {
			brace := strings.LastIndexByte(rest, '}')
			if brace < 0 {
				out.fam[name] = append(out.fam[name], value)
				continue
			}
			key = rest[:brace+1]
			rest = rest[brace+1:]
		}
		fields := strings.Fields(rest)
		if len(fields) > 0 {
			if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
				value = v
			}
		}
		if seen[name+key] {
			value = math.NaN()
		}
		seen[name+key] = true
		labels[name] = append(labels[name], key)
		out.fam[name] = append(out.fam[name], value)
	}
	if sc.Err() != nil {
		return &sample{}
	}
	for name, keys := range labels {
		sort.Strings(keys)
		out.shape[name] = strings.Join(keys, "\x00")
	}
	return out
}

// RunMetrics scrapes each ready replica every interval, updates its queue depth and passes one report to report.
func (s *Supervisor) RunMetrics(ctx context.Context, interval time.Duration, report func(wire.StatsReport)) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	prev := map[string]*sample{}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rep := s.round(ctx, prev)
		if len(rep.Replicas) > 0 && report != nil {
			report(rep)
		}
	}
}

func (s *Supervisor) round(ctx context.Context, prev map[string]*sample) wire.StatsReport {
	s.mu.Lock()
	rs := make([]*Replica, 0, len(s.replicas))
	for _, r := range s.replicas {
		rs = append(rs, r)
	}
	s.mu.Unlock()
	var rep wire.StatsReport
	live := map[string]bool{}
	for _, r := range rs {
		r.mu.Lock()
		base, key, ready, epoch, generation, inFlight := r.base, r.keys.Read, r.state == wire.StateReady, r.metricsEpoch, r.asg.Generation, r.inFlight
		r.mu.Unlock()
		if !ready || base == "" {
			continue
		}
		cur, err := s.scrape(ctx, base, key)
		if err != nil {
			continue
		}
		r.mu.Lock()
		unchanged := r.state == wire.StateReady && r.metricsEpoch == epoch && r.base == base && r.asg.Generation == generation
		r.mu.Unlock()
		if !unchanged {
			continue
		}
		cur.epoch = epoch
		live[r.id] = true
		p := prev[r.id]
		prev[r.id] = cur
		load := observations.Load{Generation: generation, ObservedAt: cur.at, TokensPerSecond: measuredRate(p, cur, produced),
			PromptTokensPerSecond: measuredRate(p, cur, prompted), Running: measuredCount(cur, famRunning), Waiting: measuredCount(cur, famWaiting)}
		if p != nil {
			load.WindowSeconds = cur.at.Sub(p.at).Seconds()
		}
		if kv, ok := cur.max(famKV); ok {
			load.KVUsage = &kv
		}
		load, _ = load.Normalize(cur.at)
		st := legacyStats(r.id, p, cur, inFlight)
		st.Observation = &load
		if load.Waiting != nil {
			r.setWaiting(*load.Waiting)
		}
		rep.Replicas = append(rep.Replicas, st)
	}
	for id := range prev {
		if !live[id] {
			delete(prev, id)
		}
	}
	sort.Slice(rep.Replicas, func(i, j int) bool { return rep.Replicas[i].ReplicaID < rep.Replicas[j].ReplicaID })
	return rep
}

func (s *Supervisor) scrape(ctx context.Context, base, key string) (*sample, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/metrics", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("metrics answered " + resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 4<<20 {
		return nil, errors.New("metrics response too large")
	}
	out := parseMetrics(strings.NewReader(string(raw)))
	out.at, out.base = time.Now(), base
	return out, nil
}

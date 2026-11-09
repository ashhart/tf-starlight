package admission

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/ashhart/tf-starlight/internal/observations"
	"github.com/ashhart/tf-starlight/internal/routing"
)

type shadowWork struct {
	raw        []byte
	generation int64
	reserved   string
}

const shadowFilter = ` AND ($5='' OR r.id::text=$5) AND NOT (r.id::text=ANY($6::text[]))
	AND (cardinality($7::text[])=0 OR (d.id::text||':'||d.revision::text)=ANY($7::text[]))`

func shadowColumns() string {
	limit := strconv.Itoa(routing.MaxShadowCandidates)
	return `,CASE WHEN count(*) OVER()<=` + limit + ` AND octet_length(r.live::text)<=4096 THEN
		jsonb_build_object('generation',r.live->'generation','observed_at',r.live->'observed_at',
		'window_seconds',r.live->'window_seconds','prompt_tokens_per_second',r.live->'prompt_tokens_per_second',
		'tokens_per_second',r.live->'tokens_per_second','running',r.live->'running','waiting',r.live->'waiting')
		ELSE '{}'::jsonb END,r.generation,CASE WHEN count(*) OVER()<=` + limit + ` THEN
		(SELECT coalesce(sum(l.reserved_tokens),0)::text FROM leases l
		WHERE l.replica_id=r.id AND l.heartbeat_at>now()-interval '` + staleSQL + `') ELSE '' END`
}

func shadowQueryArgs(req Request) []any {
	pins := make([]string, len(req.Pins))
	for i, p := range req.Pins {
		pins[i] = p.Deployment + ":" + strconv.Itoa(p.Revision)
	}
	return []any{req.Only, append([]string{}, req.Exclude...), pins}
}

func shadowDecision(req Request, local []localCand, target Target, now time.Time) routing.Decision {
	if !target.Local() {
		return routing.Decision{Reason: "cloud_target"}
	}
	if len(local) > routing.MaxShadowCandidates {
		return routing.Decision{Reason: "too_many_candidates"}
	}
	candidates := make([]routing.WorkCandidate, len(local))
	for i, c := range local {
		v := routing.WorkCandidate{Candidate: routing.Candidate{ID: c.t.ReplicaID, Slots: c.slots, Free: c.slots - c.used}}
		if c.work == nil {
			candidates[i] = v
			continue
		}
		v.Generation = c.work.generation
		if len(c.work.raw) <= 4096 {
			if err := json.Unmarshal(c.work.raw, &v.Load); err != nil {
				v.Load = observations.Load{}
			}
		}
		amount, err := strconv.ParseUint(c.work.reserved, 10, 64)
		if err == nil && amount <= observations.MaxCounter {
			v.ReservedTokens, v.ReservedKnown = int64(amount), true
		}
		candidates[i] = v
	}
	return routing.Evaluate(candidates, *req.ShadowWork, target.ReplicaID, now)
}

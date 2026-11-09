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

const shadowColumns = `,CASE WHEN octet_length(r.live::text)<=4096 THEN r.live ELSE '{}'::jsonb END,r.generation,
	(SELECT coalesce(sum(l.reserved_tokens),0)::text FROM leases l
	WHERE l.replica_id=r.id AND l.heartbeat_at>now()-interval '` + staleSQL + `')`

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

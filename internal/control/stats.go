package control

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ashhart/tf-starlight/internal/control/wire"
	"github.com/ashhart/tf-starlight/internal/httpx"
)

// stats records each replica's live load for dashboards; a node can only report its own replicas.
func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	nodeID, _, revoked, err := s.node(r)
	if err != nil || revoked {
		httpx.Write(w, httpx.Plain, errNode)
		return
	}
	var rep wire.StatsReport
	if err := decode(r, &rep); err != nil {
		httpx.Write(w, httpx.Plain, err)
		return
	}
	if len(rep.Replicas) > 256 {
		rep.Replicas = rep.Replicas[:256]
	}
	for _, st := range rep.Replicas {
		if err := s.recordStats(r.Context(), nodeID, st, time.Now()); err != nil {
			httpx.Write(w, httpx.Plain, err)
			return
		}
	}
	_, _ = s.Pool.Exec(r.Context(), "UPDATE nodes SET last_seen_at = now() WHERE id = $1", nodeID)
	w.WriteHeader(http.StatusNoContent)
}

// recordStats retains measurement time, so duplicate or delayed reports cannot renew an old observation.
func (s *Server) recordStats(ctx context.Context, node string, st wire.ReplicaStats, now time.Time) error {
	if st.Observation == nil {
		_, err := s.Pool.Exec(ctx, `UPDATE replicas SET live = '{}', live_at = NULL WHERE id = $1 AND node_id = $2
			AND NOT (live ? 'observed_at')`, st.ReplicaID, node)
		return err
	}
	load, ok := st.Observation.Normalize(now)
	if !ok || load.Generation <= 0 {
		return nil
	}
	live, err := json.Marshal(load)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `UPDATE replicas SET live = $3, live_at = $4::timestamptz
		WHERE id = $1 AND node_id = $2 AND desired = 'running' AND state = 'ready' AND generation = $5
 AND (ready_at IS NULL OR ready_at <= $4)
		AND (NOT (live ? 'observed_at') OR (live->>'observed_at')::timestamptz < $4)`,
		st.ReplicaID, node, live, load.ObservedAt, load.Generation)
	return err
}

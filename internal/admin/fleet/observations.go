package fleet

// RateValidity distinguishes measured zero rates from a sum with no valid observations.
type RateValidity struct {
	DecodeKnown bool `json:"decode_known"`
	PromptKnown bool `json:"prompt_known"`
}

// LoadValidity records which sums have at least one current, measured input.
type LoadValidity struct {
	RateValidity
	RunningKnown bool `json:"running_known"`
	WaitingKnown bool `json:"waiting_known"`
	KVKnown      bool `json:"kv_known"`
}

func freshLoad(alias string) string {
	return alias + `.live_at > now() - interval '15 seconds' AND ` + alias + `.desired = 'running' AND ` +
		alias + `.state = 'ready' AND ` + alias + `.live ? 'observed_at' AND (` + alias + `.live->>'observed_at')::timestamptz <= now() AND (` +
		alias + `.live->>'generation')::bigint = ` + alias + `.generation AND (` + alias + `.ready_at IS NULL OR ` + alias + `.live_at >= ` + alias + `.ready_at)` +
		` AND EXISTS (SELECT 1 FROM nodes observation_node WHERE observation_node.id = ` + alias + `.node_id` +
		` AND observation_node.status = 'active' AND observation_node.last_seen_at > now() - interval '90 seconds')`
}

func knownFloat(dst *float64, value *float64) bool {
	if value == nil {
		return false
	}
	*dst = *value
	return true
}

func knownInt(dst *int, value *int) bool {
	if value == nil {
		return false
	}
	*dst = *value
	return true
}

package fleet

// RateValidity distinguishes complete fresh-report coverage from an unknown aggregate.
type RateValidity struct {
	Legacy      bool `json:"legacy"`
	DecodeKnown bool `json:"decode_known"`
	PromptKnown bool `json:"prompt_known"`
}

// LoadValidity records whether every fresh report supplies each field; Legacy marks older report semantics.
type LoadValidity struct {
	RateValidity
	RunningKnown bool `json:"running_known"`
	WaitingKnown bool `json:"waiting_known"`
	KVKnown      bool `json:"kv_known"`
}

func freshLoad(alias string) string {
	modern := alias + `.live ? 'observed_at' AND (` + alias + `.live->>'observed_at')::timestamptz <= now() AND (` +
		alias + `.live->>'generation')::bigint = ` + alias + `.generation AND (` + alias + `.ready_at IS NULL OR ` + alias + `.live_at >= ` + alias + `.ready_at)`
	return alias + `.live_at > now() - interval '15 seconds' AND ` + alias + `.desired = 'running' AND ` + alias + `.state = 'ready' AND (` +
		legacyLoad(alias) + ` OR (` + modern + `)) AND EXISTS (SELECT 1 FROM nodes observation_node WHERE observation_node.id = ` + alias + `.node_id` +
		` AND observation_node.status = 'active' AND observation_node.last_seen_at > now() - interval '90 seconds')`
}

func legacyLoad(alias string) string {
	return `(` + alias + `.live->>'legacy'='true' OR (NOT (` + alias + `.live ? 'observed_at') AND ` + alias + `.live ? 'replica_id'))`
}

func completeAggregate(alias, field, pgType, aggregate string) string {
	value := alias + `.live->>'` + field + `'`
	return `CASE WHEN count(*)>0 AND count(` + value + `)=count(*) THEN ` + aggregate + `((` + value + `)::` + pgType + `) END`
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

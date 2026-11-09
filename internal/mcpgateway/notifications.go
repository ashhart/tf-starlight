package mcpgateway

import (
	"encoding/json"
	"math"
)

func validProgress(raw, token json.RawMessage) bool {
	var p map[string]json.RawMessage
	if json.Unmarshal(raw, &p) != nil || p == nil || fieldAliases(p, "progressToken", "progress", "total", "message") || !sameID(p["progressToken"], token) {
		return false
	}
	var progress float64
	if string(p["progress"]) == "null" || json.Unmarshal(p["progress"], &progress) != nil || math.IsInf(progress, 0) || progress < 0 {
		return false
	}
	if total, ok := p["total"]; ok {
		var n float64
		if string(total) == "null" || json.Unmarshal(total, &n) != nil || math.IsInf(n, 0) || n < 0 {
			return false
		}
	}
	if message, ok := p["message"]; ok {
		var text string
		if string(message) == "null" || json.Unmarshal(message, &text) != nil {
			return false
		}
	}
	return true
}

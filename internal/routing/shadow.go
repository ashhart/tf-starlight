package routing

import (
	"math"
	"time"

	"github.com/ashhart/tf-starlight/internal/observations"
)

// Work contains estimates, not measured tokens or confirmed prefix-cache credit.
type Work struct {
	Prompt, Completion int64
}

// WorkCandidate adds observed phase rates and outstanding reservations to an already eligible target.
type WorkCandidate struct {
	Candidate
	Load           observations.Load
	Generation     int64
	ReservedTokens int64
	ReservedKnown  bool
}

// Decision compares an advisory effort score with the actual committed target; it never chooses a live route.
type Decision struct {
	Reason          string
	ActualID        string
	ProposedID      string
	ActualSeconds   float64
	ProposedSeconds float64
}

// MaxShadowCandidates bounds one comparison without truncating the real admission candidate set.
const MaxShadowCandidates = 128

// Evaluate requires every free local candidate to be comparable; missing measurements do not favor another target.
func Evaluate(candidates []WorkCandidate, work Work, actual string, now time.Time) Decision {
	d := Decision{ActualID: actual}
	if len(candidates) > MaxShadowCandidates {
		return Decision{ActualID: actual, Reason: "too_many_candidates"}
	}
	if work.Prompt < 0 || work.Completion < 0 || work.Prompt > observations.MaxCounter-work.Completion ||
		work.Prompt+work.Completion == 0 {
		return Decision{ActualID: actual, Reason: "invalid_work"}
	}
	free, found := 0, false
	seen := map[string]bool{}
	for _, c := range candidates {
		if c.Free <= 0 {
			continue
		}
		free++
		if seen[c.ID] {
			return Decision{ActualID: actual, Reason: "invalid_capacity"}
		}
		seen[c.ID] = true
		score, reason := effort(c, work, now)
		if reason != "" {
			return Decision{ActualID: actual, Reason: reason}
		}
		if c.ID == actual {
			d.ActualSeconds, found = score, true
		}
		if d.ProposedID == "" || score < d.ProposedSeconds || score == d.ProposedSeconds && c.ID < d.ProposedID {
			d.ProposedID, d.ProposedSeconds = c.ID, score
		}
	}
	switch {
	case !found:
		return Decision{ActualID: actual, Reason: "missing_actual"}
	case free < 2:
		d.Reason = "no_alternative"
	default:
		d.Reason = "ready"
	}
	return d
}

func effort(c WorkCandidate, work Work, now time.Time) (float64, string) {
	if c.ID == "" || c.Slots <= 0 || c.Free > c.Slots {
		return 0, "invalid_capacity"
	}
	load, ok := c.Load.Normalize(now)
	if !ok || !load.Fresh(now) || load.Generation <= 0 || load.Generation != c.Generation || load.WindowSeconds <= 0 {
		return 0, "unknown_observation"
	}
	if !c.ReservedKnown || c.ReservedTokens < 0 || c.ReservedTokens > observations.MaxCounter {
		return 0, "unknown_backlog"
	}
	if load.Running == nil || load.Waiting == nil {
		return 0, "unknown_backlog"
	}
	if int64(*load.Running)+int64(*load.Waiting) > int64(c.Slots-c.Free) {
		return 0, "untracked_work"
	}
	prompt, decode := positive(load.PromptTokensPerSecond), positive(load.TokensPerSecond)
	if work.Prompt > 0 && prompt == 0 || work.Completion > 0 && decode == 0 || c.ReservedTokens > 0 && (prompt == 0 || decode == 0) {
		return 0, "unknown_rate"
	}
	score := 0.0
	if work.Prompt > 0 {
		score += float64(work.Prompt) / prompt
	}
	if work.Completion > 0 {
		score += float64(work.Completion) / decode
	}
	if c.ReservedTokens > 0 {
		score += float64(c.ReservedTokens) / min(prompt, decode)
	}
	if math.IsNaN(score) || math.IsInf(score, 0) || score <= 0 || score > (24*time.Hour).Seconds() {
		return 0, "invalid_score"
	}
	return score, ""
}

func positive(v *float64) float64 {
	if v == nil || *v <= 0 {
		return 0
	}
	return *v
}

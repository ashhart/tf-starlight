// Package observations represents measured replica work without inventing values for missing samples.
package observations

import (
	"math"
	"time"
)

// MaxAge bounds the age of work observations used by dashboards and future routing consumers.
const MaxAge = 15 * time.Second

// MaxCounter is the largest integer whose increments remain exact in the float64 scrape representation.
const MaxCounter = 1<<53 - 1

// MaxRate bounds a counter delta over the smallest duration represented by the node's nanosecond clock.
const MaxRate = float64(MaxCounter) * float64(time.Second)

// Load holds optional measurements; nil means unknown, while a pointer to zero is measured idle work.
type Load struct {
	Generation            int64     `json:"generation,omitempty"`
	ObservedAt            time.Time `json:"observed_at,omitzero"`
	WindowSeconds         float64   `json:"window_seconds,omitempty"`
	TokensPerSecond       *float64  `json:"tokens_per_second,omitempty"`
	PromptTokensPerSecond *float64  `json:"prompt_tokens_per_second,omitempty"`
	Running               *int      `json:"running,omitempty"`
	Waiting               *int      `json:"waiting,omitempty"`
	KVUsage               *float64  `json:"kv_usage,omitempty"`
}

// Fresh requires an observation timestamp and a nonnegative age shorter than MaxAge.
func (l Load) Fresh(now time.Time) bool {
	age := now.Sub(l.ObservedAt)
	return !l.ObservedAt.IsZero() && age >= 0 && age < MaxAge
}

// Normalize removes invalid fields and rejects missing, expired or excessively future timestamps.
func (l Load) Normalize(now time.Time) (Load, bool) {
	l.ObservedAt = l.ObservedAt.UTC().Truncate(time.Microsecond)
	if l.ObservedAt.IsZero() || now.Sub(l.ObservedAt) >= MaxAge || l.ObservedAt.Sub(now) > 5*time.Second {
		return Load{}, false
	}
	l.TokensPerSecond = nonnegative(l.TokensPerSecond)
	l.PromptTokensPerSecond = nonnegative(l.PromptTokensPerSecond)
	if l.TokensPerSecond != nil && *l.TokensPerSecond > MaxRate {
		l.TokensPerSecond = nil
	}
	if l.PromptTokensPerSecond != nil && *l.PromptTokensPerSecond > MaxRate {
		l.PromptTokensPerSecond = nil
	}
	l.KVUsage = nonnegative(l.KVUsage)
	if !finite(l.WindowSeconds) || l.WindowSeconds <= 0 || l.WindowSeconds > MaxAge.Seconds() {
		l.WindowSeconds, l.TokensPerSecond, l.PromptTokensPerSecond = 0, nil, nil
	}
	if l.KVUsage != nil && *l.KVUsage > 1 {
		l.KVUsage = nil
	}
	if l.Running != nil && (*l.Running < 0 || *l.Running > math.MaxInt32) {
		l.Running = nil
	}
	if l.Waiting != nil && (*l.Waiting < 0 || *l.Waiting > math.MaxInt32) {
		l.Waiting = nil
	}
	return l, true
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

func nonnegative(v *float64) *float64 {
	if v == nil || !finite(*v) {
		return nil
	}
	return v
}

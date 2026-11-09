package observations

import (
	"math"
	"testing"
	"time"
)

func TestNormalizeKeepsMeasuredZeroAndRemovesInvalidValues(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	zero, negative, nan, tooMuch := 0.0, -1.0, math.NaN(), 1.1
	running, waiting := 0, -1
	l, ok := (Load{ObservedAt: now, WindowSeconds: 5, TokensPerSecond: &zero, PromptTokensPerSecond: &nan,
		Running: &running, Waiting: &waiting, KVUsage: &tooMuch}).Normalize(now)
	if !ok || !l.Fresh(now) || l.TokensPerSecond == nil || *l.TokensPerSecond != 0 || l.Running == nil {
		t.Fatalf("measured zero lost: %+v, %v", l, ok)
	}
	if l.PromptTokensPerSecond != nil || l.Waiting != nil || l.KVUsage != nil {
		t.Fatalf("invalid readings survived: %+v", l)
	}
	large := int64(math.MaxInt32) + 1
	count := int(large)
	invalidCount, _ := (Load{ObservedAt: now, Running: &count, Waiting: &count}).Normalize(now)
	if invalidCount.Running != nil || invalidCount.Waiting != nil {
		t.Fatal("out-of-range database request counts accepted")
	}
	for _, value := range []float64{negative, math.Inf(1), math.Inf(-1), nan, math.MaxFloat64} {
		l, _ := (Load{ObservedAt: now, WindowSeconds: 5, TokensPerSecond: &value}).Normalize(now)
		if l.TokensPerSecond != nil {
			t.Fatalf("invalid rate survived: %v", value)
		}
	}
}

func TestNormalizeRequiresAValidClockAndRateWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, at := range []time.Time{{}, now.Add(-MaxAge), now.Add(6 * time.Second)} {
		if _, ok := (Load{ObservedAt: at}).Normalize(now); ok {
			t.Fatalf("accepted timestamp %v", at)
		}
	}
	zero := 0.0
	for _, window := range []float64{0, -1, math.NaN(), math.Inf(1), 16} {
		l, ok := (Load{ObservedAt: now, WindowSeconds: window, TokensPerSecond: &zero}).Normalize(now)
		if !ok || l.TokensPerSecond != nil {
			t.Fatalf("window %v: %+v, %v", window, l, ok)
		}
	}
	if (Load{ObservedAt: now.Add(time.Second)}).Fresh(now) {
		t.Fatal("future data is current")
	}
	if (Load{ObservedAt: now}).Fresh(now.Add(MaxAge)) {
		t.Fatal("expired data is current")
	}
}

package gateway

import (
	"errors"
	"testing"
	"time"
)

func validateStreamGateOrder(start time.Time, headers time.Duration, times *streamTimes, clientOutput time.Time) error {
	if headers > times.data.elapsed || times.data.elapsed > times.output.elapsed || times.output.elapsed > times.flush.elapsed ||
		clientOutput.Before(start.Add(times.output.elapsed)) {
		return errors.New("gateway and client clock order is invalid")
	}
	return nil
}

func TestStreamGateRejectsInvertedGatewayAndClientClocks(t *testing.T) {
	start := time.Unix(100, 0)
	for _, fixture := range []struct {
		name                         string
		headers, data, output, flush time.Duration
		client                       time.Duration
		valid                        bool
	}{
		{"ordered", 1, 2, 3, 4, 5, true},
		{"headers after data", 3, 2, 3, 4, 5, false},
		{"data after output", 1, 4, 3, 4, 5, false},
		{"output after flush", 1, 2, 5, 4, 6, false},
		{"client before gateway output", 1, 2, 4, 5, 3, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			times := streamTimes{data: seenTime{elapsed: fixture.data * time.Millisecond, seen: true},
				output: seenTime{elapsed: fixture.output * time.Millisecond, seen: true},
				flush:  seenTime{elapsed: fixture.flush * time.Millisecond, seen: true}}
			err := validateStreamGateOrder(start, fixture.headers*time.Millisecond, &times, start.Add(fixture.client*time.Millisecond))
			if (err == nil) != fixture.valid {
				t.Fatalf("clock fixture accepted=%v, want %v", err == nil, fixture.valid)
			}
		})
	}
}

package scheduler

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/ashhart/tf-starlight/internal/accounting"
)

func TestAccountingFailuresAreObservedAndLoggedWithoutPayloads(t *testing.T) {
	var log bytes.Buffer
	var observed []accounting.MaintenanceEvent
	h := &housekeeper{log: slog.New(slog.NewTextHandler(&log, nil)),
		accounting: func(e accounting.MaintenanceEvent) { observed = append(observed, e) }}
	h.reportAccounting(accounting.MaintenanceEvent{Stage: "drain", Count: 2, Failed: 1,
		Err: errors.New("private connection payload")})
	h.reportAccounting(accounting.MaintenanceEvent{Stage: "prune", Count: 6001, Batches: 7, Limited: true})
	if len(observed) != 2 || observed[0].Failed != 1 || observed[1].Count != 6001 {
		t.Fatalf("maintenance failures/results were discarded: %+v", observed)
	}
	text := log.String()
	if !strings.Contains(text, "accounting maintenance failed") || !strings.Contains(text, "error_class=other") ||
		!strings.Contains(text, "budget exhausted") || strings.Contains(text, "private connection payload") {
		t.Fatalf("maintenance logging is missing or exposes payload: %q", text)
	}
}

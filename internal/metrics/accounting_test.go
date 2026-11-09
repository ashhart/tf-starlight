package metrics

import (
	"errors"
	"strings"
	"testing"

	"github.com/ashhart/tf-starlight/internal/accounting"
)

func TestAccountingMaintenanceHealthPreservesFailedSnapshotAsUnknown(t *testing.T) {
	s := New()
	observe := s.RegisterAccountingMaintenance()
	observe(accounting.MaintenanceEvent{Stage: "backlog", Backlog: &accounting.Backlog{
		Pending: 7, Expired: 100000, ExpiredCapped: true, OldestPending: 90}})
	observe(accounting.MaintenanceEvent{Stage: "drain", Count: 2, Failed: 1, Batches: 1, Err: errors.New("private connection payload")})
	observe(accounting.MaintenanceEvent{Stage: "prune", Count: 6001, Batches: 7, Limited: true})
	observe(accounting.MaintenanceEvent{Stage: "backlog", Err: errors.New("private connection payload")})
	families, err := s.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{}
	for _, family := range families {
		for _, metric := range family.Metric {
			key := family.GetName()
			for _, label := range metric.Label {
				if strings.Contains(label.GetValue(), "private") {
					t.Fatal("error payload became a metric label")
				}
				key += "/" + label.GetName() + "=" + label.GetValue()
			}
			if metric.Gauge != nil {
				values[key] = metric.GetGauge().GetValue()
			} else if metric.Counter != nil {
				values[key] = metric.GetCounter().GetValue()
			}
		}
	}
	for key, want := range map[string]float64{
		"starlight_accounting_backlog_available":                    0,
		"starlight_accounting_backlog/kind=pending_audit":           7,
		"starlight_accounting_backlog/kind=expired_receipts":        100000,
		"starlight_accounting_backlog_capped/kind=expired_receipts": 1,
		"starlight_accounting_oldest_pending_seconds":               90,
		"starlight_accounting_audit_failed_attempts_total":          1,
		"starlight_accounting_maintenance_rows_total/stage=prune":   6001,
	} {
		if got, ok := values[key]; !ok || got != want {
			t.Fatalf("%s=%v present=%v want=%v", key, got, ok, want)
		}
	}
}

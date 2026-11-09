package e2e_test

import (
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/config"
)

func TestReplicaObservationsNegotiateOnTheGovernedFleetAndExportOnlyWhenEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			f := startStarlight(t, func(c *config.Config) { c.Metrics.ReplicaObservations = enabled })
			f.enrolNode(f.nodeURL)
			deadline := time.Now().Add(12 * time.Second)
			observed := false
			for time.Now().Before(deadline) {
				if err := f.pool.QueryRow(f.ctx, "SELECT EXISTS(SELECT 1 FROM replicas WHERE live ? 'observed_at' AND live ? 'running')").Scan(&observed); err != nil {
					t.Fatal(err)
				}
				if observed {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			if !observed {
				t.Fatal("node did not negotiate and post a qualified observation")
			}
			families, err := f.starlight.Metrics.Registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, family := range families {
				if family.GetName() == "starlight_replica_running_requests" {
					count = len(family.Metric)
					if count != 1 || family.Metric[0].Gauge.GetValue() != 0 {
						t.Fatal("idle replica gauge is wrong")
					}
				}
			}
			want := 0
			if enabled {
				want = 1
			}
			if count != want {
				t.Fatalf("exported %d replicas, want %d", count, want)
			}
		})
	}
}

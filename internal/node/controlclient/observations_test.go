package controlclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/control/wire"
	"github.com/ashhart/tf-starlight/internal/observations"
)

func TestStatsMetadataIsNegotiatedWithoutChangingTheOfferedReport(t *testing.T) {
	for _, supported := range []bool{false, true} {
		var mu sync.Mutex
		var got []bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == wire.PathSync {
				_ = json.NewEncoder(w).Encode(wire.SyncResponse{ObservationStats: supported})
				return
			}
			var rep wire.StatsReport
			if json.NewDecoder(r.Body).Decode(&rep) != nil || len(rep.Replicas) != 1 {
				t.Error("bad stats report")
				w.WriteHeader(400)
				return
			}
			mu.Lock()
			got = append(got, rep.Replicas[0].Observation != nil)
			mu.Unlock()
			w.WriteHeader(204)
		}))
		u, _ := url.Parse(srv.URL)
		c := &Client{HTTP: srv.Client(), Base: u}
		rep := wire.StatsReport{Replicas: []wire.ReplicaStats{{ReplicaID: "replica", Observation: &observations.Load{ObservedAt: time.Now()}}}}
		if err := c.Stats(context.Background(), rep); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Sync(context.Background(), wire.SyncRequest{}); err != nil {
			t.Fatal(err)
		}
		if err := c.Stats(context.Background(), rep); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		mu.Lock()
		if len(got) != 2 || got[0] || got[1] != supported {
			t.Fatalf("negotiated reports %v, supported %v", got, supported)
		}
		mu.Unlock()
		if rep.Replicas[0].Observation == nil {
			t.Fatal("caller report was mutated")
		}
	}
}

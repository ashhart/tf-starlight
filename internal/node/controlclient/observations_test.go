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
			if rep.Replicas[0].Running != 3 || rep.Replicas[0].KVUsage != 1 {
				t.Error("legacy fields changed during negotiation")
			}
			mu.Lock()
			got = append(got, rep.Replicas[0].Observation != nil)
			mu.Unlock()
			w.WriteHeader(204)
		}))
		u, _ := url.Parse(srv.URL)
		c := &Client{HTTP: srv.Client(), Base: u}
		rep := wire.StatsReport{Replicas: []wire.ReplicaStats{{ReplicaID: "replica", Running: 3, KVUsage: 1, Observation: &observations.Load{ObservedAt: time.Now()}}}}
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

func TestMixedVersionListenersFallBackToLegacyInsteadOfLosingTheReport(t *testing.T) {
	var mu sync.Mutex
	var got []bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == wire.PathSync {
			_ = json.NewEncoder(w).Encode(wire.SyncResponse{ObservationStats: true})
			return
		}
		var rep wire.StatsReport
		if json.NewDecoder(r.Body).Decode(&rep) != nil || len(rep.Replicas) != 1 {
			w.WriteHeader(400)
			return
		}
		metadata := rep.Replicas[0].Observation != nil
		mu.Lock()
		got = append(got, metadata)
		mu.Unlock()
		if metadata {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_body","message":"unknown observation field"}`))
			return
		}
		if rep.Replicas[0].Running != 3 || rep.Replicas[0].KVUsage != 1 {
			t.Error("legacy fallback changed its values")
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	c := &Client{HTTP: server.Client(), Base: u}
	if _, err := c.Sync(context.Background(), wire.SyncRequest{}); err != nil {
		t.Fatal(err)
	}
	report := wire.StatsReport{Replicas: []wire.ReplicaStats{{ReplicaID: "replica", Running: 3, KVUsage: 1, Observation: &observations.Load{ObservedAt: time.Now()}}}}
	if err := c.Stats(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	if err := c.Stats(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 || !got[0] || got[1] || got[2] {
		t.Fatalf("mixed-version reports %v", got)
	}
	if report.Replicas[0].Observation == nil {
		t.Fatal("caller observation was mutated during fallback")
	}
}

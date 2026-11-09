package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/observations"
	"github.com/ashhart/tf-starlight/internal/routing"
	"github.com/ashhart/tf-starlight/internal/testutil"
)

func TestShadowMetadataTransfersOnlyBoundedKnownFields(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	for _, count := range []int{1, routing.MaxShadowCandidates, routing.MaxShadowCandidates + 1} {
		payload, err := json.Marshal(map[string]any{"generation": 1, "observed_at": time.Now().UTC(),
			"window_seconds": 1, "prompt_tokens_per_second": 100, "tokens_per_second": 100,
			"running": 0, "waiting": 0, "unexpected_private_field": "metadata-private-canary" + strings.Repeat("x", 3500)})
		if err != nil {
			t.Fatal(err)
		}
		rows, err := pool.Query(ctx, `WITH r AS (SELECT md5(n::text)::uuid AS id,1 AS generation,$1::jsonb AS live
			FROM generate_series(1,$2::int) n) SELECT 1`+shadowColumns()+` FROM r`, payload, count)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for rows.Next() {
			var one, generation int
			var raw []byte
			var reserved string
			if err := rows.Scan(&one, &raw, &generation, &reserved); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if len(raw) > 512 || strings.Contains(string(raw), "metadata-private-canary") || strings.Contains(string(raw), "unexpected_private_field") {
				rows.Close()
				t.Fatal("shadow snapshot transferred unknown or excessive payload fields")
			}
			if count > routing.MaxShadowCandidates {
				if string(raw) != "{}" || reserved != "" {
					rows.Close()
					t.Fatal("unavailable comparison fetched metadata or reservation totals beyond its cap")
				}
			} else {
				var load observations.Load
				if json.Unmarshal(raw, &load) != nil || load.Generation != 1 || load.TokensPerSecond == nil || reserved != "0" {
					rows.Close()
					t.Fatal("bounded metadata lost a required phase field or known zero reservation")
				}
			}
			seen++
		}
		rows.Close()
		if err := rows.Err(); err != nil || seen != count {
			t.Fatalf("metadata cap truncated actual candidates: seen=%d want=%d: %v", seen, count, err)
		}
	}
}

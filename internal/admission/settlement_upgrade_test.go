package admission_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/admission"
	"github.com/ashhart/tf-starlight/internal/db"
	"github.com/ashhart/tf-starlight/internal/levels"
)

func TestSettlementFixUpgradesAnExistingSchemaNine(t *testing.T) {
	f := newFleet(t, 1)
	migrations, err := db.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	old := migrations[8].SQL
	start := strings.Index(old, "CREATE FUNCTION starlight_settle(r jsonb)")
	if start < 0 {
		t.Fatal("schema9 settlement definition missing")
	}
	f.exec(t, strings.Replace(old[start:], "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION", 1))
	f.exec(t, "DELETE FROM schema_migrations WHERE version=10")
	applied, err := db.Migrate(context.Background(), f.pool)
	if err != nil || len(applied) != 1 || applied[0] != "0010_settlement_once.sql" {
		t.Fatalf("upgrading existing settlement: %v, %v", applied, err)
	}
	a := f.admitter()
	lease, err := a.Admit(context.Background(), f.request(levels.Standard, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := a.Settle(context.Background(), lease, admission.Usage{Prompt: 3, Completion: 4, Observed: true}, record()); err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM usage_events").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("upgraded settlement usage rows=%d, err=%v", rows, err)
	}
}

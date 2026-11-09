package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ashhart/tf-starlight/internal/db"
	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/testutil"
)

func TestTerminalMigrationLockWaitIsBoundedAndRetryable(t *testing.T) {
	pool := schemaTen(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	_, err = blocker.Exec(ctx, `INSERT INTO leases(id,request_id,gateway_id,model_id,scopes,priority,deadline)
		VALUES($1,'migration-lock',$2,$3,'{}','standard',now())`, ids.New(), ids.New(), ids.New())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Migrate(ctx, pool)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "55P03" {
		t.Fatalf("hot-table migration did not fail on its own lock timeout: %v", err)
	}
	var top int
	var table *string
	if err := pool.QueryRow(ctx, `SELECT max(version),to_regclass('terminal_leases')::text FROM schema_migrations`).
		Scan(&top, &table); err != nil || top != 10 || table != nil {
		t.Fatalf("lock timeout leaked a partial migration: schema=%d table=%v: %v", top, table, err)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if applied, err := db.Migrate(ctx, pool); err != nil || len(applied) != 1 {
		t.Fatalf("lock-timeout retry failed: %v, %v", applied, err)
	}
}

func TestLeasesHaveNoForeignKeyCascadePath(t *testing.T) {
	pool := testutil.DB(t)
	var constraints int
	err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_constraint WHERE contype='f'
  AND (conrelid='leases'::regclass OR confrelid='leases'::regclass)`).Scan(&constraints)
	if err != nil || constraints != 0 {
		t.Fatalf("lease delete inventory changed: foreign-key paths=%d: %v", constraints, err)
	}
}

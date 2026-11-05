//go:build !windows

package e2e_test

import (
	"context"
	"net/http"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ashhart/tf-starlight/internal/crypto/keyring"
	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/testutil"
)

// TestHighAvailabilityDrills is the qualification gate of that name, with real processes; STARLIGHT_DRILL=1 runs it.
func TestHighAvailabilityDrills(t *testing.T) {
	if os.Getenv("STARLIGHT_DRILL") == "" {
		t.Skip("set STARLIGHT_DRILL=1 to kill and restore a real installation")
	}
	for _, tool := range []string{"pg_dump", "pg_restore"} {
		if _, err := osexec.LookPath(tool); err != nil {
			t.Fatalf("%s must be on PATH and match the server's major version: %v", tool, err)
		}
	}
	d := newDrill(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if out, err := d.run(d.env(d.url, d.master, "127.0.0.1:1"), d.tool("starlight"), "init", "--org", "Example Corp"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	f := &fleet{t: t, ctx: ctx, pool: d.pool}
	f.catalogue()

	a := d.serve("first", d.url, d.master)
	d.waitFor("the first instance to lead", 30*time.Second, func() bool { return a.epoch() > 0 })
	b := d.serve("second", d.url, d.master)
	if e := b.epoch(); e != 0 {
		t.Fatalf("a second instance reports epoch %d while the first leads", e)
	}
	d.enrol(a)
	d.waitFor("the replica to become ready", 2*time.Minute, func() bool { return d.ready("qwen-mac") })
	placed := d.liveReplicas("qwen-mac")
	peaks := watchPlacements(ctx, f)
	if code, body := d.chat(f, b); code != http.StatusOK {
		t.Fatalf("chat through the second instance: %d %s", code, body)
	}

	// The leader dies. Requests keep flowing, and a deployment added meanwhile waits for the next leader.
	first := a.epoch()
	killed := time.Now()
	a.kill()
	if code, body := d.chat(f, b); code != http.StatusOK {
		t.Fatalf("chat with no leader: %d %s", code, body)
	}
	exec(t, d.pool, `INSERT INTO deployments (id, model_id, name, spec, mode, min_replicas)
		SELECT $1, model_id, 'qwen-during-failover', spec, 'warm', 1 FROM deployments WHERE name = 'qwen-mac'`, ids.New())
	d.waitFor("a new epoch after killing the leader", takeoverBudget, func() bool { return b.epoch() > first })
	t.Logf("the leader was killed at epoch %d; epoch %d took over after %s", first, b.epoch(), time.Since(killed).Round(10*time.Millisecond))
	if e := b.epoch(); e != first+1 {
		t.Fatalf("epoch %d after %d", e, first)
	}
	d.waitFor("the deployment added during failover to be placed", time.Minute, func() bool { return d.ready("qwen-during-failover") })
	time.Sleep(3 * 5 * time.Second)
	d.expectOnePlacement(peaks, "qwen-mac", "qwen-during-failover")
	if now := d.liveReplicas("qwen-mac"); now[0] != placed[0] {
		t.Fatalf("failover replaced replica %s with %s", placed[0], now[0])
	}
	if code, body := d.chat(f, b); code != http.StatusOK {
		t.Fatalf("chat after failover: %d %s", code, body)
	}

	// A leader that only pauses, as a frozen VM would: its successor takes over and it stands down when it wakes.
	c := d.serve("third", d.url, d.master)
	second := b.epoch()
	paused := time.Now()
	if err := b.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	d.waitFor("a new epoch while the leader is paused", takeoverBudget, func() bool { return c.epoch() > second })
	t.Logf("the leader was paused at epoch %d; epoch %d took over after %s", second, c.epoch(), time.Since(paused).Round(10*time.Millisecond))
	if err := b.cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	d.waitFor("the paused leader to stand down", 15*time.Second, func() bool { return b.epoch() == 0 })
	var held int64
	if err := d.pool.QueryRow(ctx, "SELECT epoch FROM controller_leader WHERE id").Scan(&held); err != nil {
		t.Fatal(err)
	}
	if e := c.epoch(); e != second+1 || held != e {
		t.Fatalf("the third instance reports epoch %d and the lease says %d, after %d", e, held, second)
	}
	time.Sleep(3 * 5 * time.Second)
	d.expectOnePlacement(peaks, "qwen-mac", "qwen-during-failover")

	// The site is lost. Restore the backup into a new database, with the same master key, and check it as the docs say.
	dump := filepath.Join(d.dir, "starlight.dump")
	if out, err := d.run(os.Environ(), "pg_dump", "--format=custom", "--file="+dump, d.url); err != nil {
		t.Fatalf("pg_dump: %v\n%s", err, out)
	}
	b.kill()
	c.kill()
	restored := d.createDatabase()
	if out, err := d.run(os.Environ(), "pg_restore", "--no-owner", "--exit-on-error", "--dbname="+restored, dump); err != nil {
		t.Fatalf("pg_restore: %v\n%s", err, out)
	}
	other := filepath.Join(d.dir, "other-key")
	if err := os.WriteFile(other, []byte(keyring.GenerateMasterKey()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := d.run(d.env(restored, other, "127.0.0.1:1"), d.tool("starlight"), "doctor"); err == nil || !strings.Contains(out, "keys") {
		t.Fatalf("the doctor passed a restore under the wrong master key: %v\n%s", err, out)
	}
	r := d.serve("restored", restored, d.master)
	d.waitFor("the restored instance to lead", 30*time.Second, func() bool { return r.epoch() > 0 })
	env := d.env(restored, d.master, r.public)
	out, err := d.run(env, d.tool("starlight"), "doctor")
	t.Logf("doctor on the restored database:\n%s", out)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	out, err = d.run(env, d.tool("starlight"), "audit", "verify")
	t.Logf("audit verify on the restored database: %s", out)
	if err != nil || !regexp.MustCompile(`^verified [1-9]`).MatchString(out) {
		t.Fatalf("audit verify: %v", err)
	}
	d.waitFor("a request through the restored instance", time.Minute, func() bool {
		code, _ := d.chat(f, r)
		return code == http.StatusOK
	})
}

// createDatabase makes an empty database for a restore and returns its URL.
func (d *drill) createDatabase() string {
	d.t.Helper()
	ctx := context.Background()
	admin := os.Getenv(testutil.EnvURL)
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		d.t.Fatal(err)
	}
	defer conn.Close(ctx)
	name := "starlight_r_" + strings.ReplaceAll(ids.New(), "-", "")[:24]
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		d.t.Fatal(err)
	}
	d.t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(admin)
	if err != nil {
		d.t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

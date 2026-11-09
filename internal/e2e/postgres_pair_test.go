//go:build !windows

package e2e_test

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type postgresPair struct {
	t                              *testing.T
	primary, standby               string
	primaryAddress, standbyAddress string
	tools                          map[string]string
	endpoint                       *postgresEndpoint
}

func newPostgresPair(t *testing.T) *postgresPair {
	t.Helper()
	p := &postgresPair{t: t, tools: map[string]string{}}
	versionPattern := regexp.MustCompile(`PostgreSQL\) (17\.[0-9]+)`)
	version := ""
	for _, name := range []string{"initdb", "pg_ctl", "pg_basebackup"} {
		path, err := osexec.LookPath(name)
		if err != nil {
			t.Fatalf("the opt-in database drill requires Postgres17 %s on PATH", name)
		}
		p.tools[name] = path
		out := p.run(name, "--version")
		match := versionPattern.FindStringSubmatch(out)
		if len(match) != 2 || version != "" && match[1] != version {
			t.Fatal("database drill tools must be the same Postgres17 version")
		}
		version = match[1]
	}
	t.Logf("isolated Postgres tools %s", version)
	dir := t.TempDir()
	p.primary, p.standby = filepath.Join(dir, "primary"), filepath.Join(dir, "standby")
	p.primaryAddress, p.standbyAddress = freeAddr(t), freeAddr(t)
	for p.standbyAddress == p.primaryAddress {
		p.standbyAddress = freeAddr(t)
	}
	t.Cleanup(p.close)
	p.run("initdb", "-D", p.primary, "-U", "starlight_test", "--auth=trust", "--no-locale", "--encoding=UTF8")
	p.start(p.primary, p.primaryAddress)
	primary := p.connect(p.primaryAddress)
	defer primary.Close(context.Background())
	base, err := url.Parse(postgresURL(p.primaryAddress))
	if err != nil {
		t.Fatal(err)
	}
	query := base.Query()
	query.Set("application_name", "starlight_standby")
	base.RawQuery = query.Encode()
	p.run("pg_basebackup", "-D", p.standby, "--dbname="+base.String(), "--write-recovery-conf", "--wal-method=stream", "--checkpoint=fast", "--no-password")
	p.start(p.standby, p.standbyAddress)
	for _, sql := range []string{
		"ALTER SYSTEM SET synchronous_commit='remote_apply'",
		"ALTER SYSTEM SET synchronous_standby_names='FIRST 1 (starlight_standby)'",
		"SELECT pg_reload_conf()",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := primary.Exec(ctx, sql)
		cancel()
		if err != nil {
			t.Fatalf("synchronous replication setup: %v", err)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		var mode string
		var synchronous bool
		err := primary.QueryRow(ctx, "SHOW synchronous_commit").Scan(&mode)
		if err == nil {
			err = primary.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_replication WHERE application_name='starlight_standby' AND state='streaming' AND sync_state='sync')").Scan(&synchronous)
		}
		cancel()
		if err == nil && mode == "remote_apply" && synchronous {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("standby did not become synchronous; the drill cannot assert acknowledged-write durability")
		}
		time.Sleep(25 * time.Millisecond)
	}
	p.endpoint = newPostgresEndpoint(t, p.primaryAddress)
	return p
}

func postgresURL(address string) string {
	return "postgres://starlight_test@" + address + "/postgres?sslmode=disable&connect_timeout=2"
}

func (p *postgresPair) URL() string { return postgresURL(p.endpoint.ln.Addr().String()) }

func (p *postgresPair) run(name string, args ...string) string {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := osexec.CommandContext(ctx, p.tools[name], args...)
	cmd.Env = append(os.Environ(), "PGCONNECT_TIMEOUT=2")
	out, err := cmd.CombinedOutput()
	if err != nil {
		p.t.Fatalf("isolated %s failed: %v\n%s", name, err, out)
	}
	return string(out)
}

func (p *postgresPair) start(dir, address string) {
	p.t.Helper()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		p.t.Fatal(err)
	}
	options := "-h 127.0.0.1 -p " + port + " -c unix_socket_directories= -c max_connections=100 -c wal_level=replica -c max_wal_senders=4 -c wal_receiver_status_interval=1s"
	p.run("pg_ctl", "-D", dir, "-l", dir+".log", "-o", options, "-t", "20", "-w", "start")
}

func (p *postgresPair) connect(address string) *pgx.Conn {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, postgresURL(address))
	if err != nil {
		p.t.Fatalf("isolated database connection: %v", err)
	}
	return conn
}

func (p *postgresPair) stopPrimary() {
	p.t.Helper()
	p.run("pg_ctl", "-D", p.primary, "-m", "immediate", "-t", "20", "-w", "stop")
}

func (p *postgresPair) promote() {
	p.t.Helper()
	p.run("pg_ctl", "-D", p.standby, "-t", "20", "-w", "promote")
	conn := p.connect(p.standbyAddress)
	defer conn.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var recovery bool
	if err := conn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&recovery); err != nil || recovery {
		p.t.Fatal("standby did not become writable")
	}
	p.endpoint.retarget(p.standbyAddress)
}

func (p *postgresPair) close() {
	if p.endpoint != nil {
		p.endpoint.close()
	}
	for _, dir := range []string{p.primary, p.standby} {
		if dir == "" {
			continue
		}
		for _, mode := range []string{"fast", "immediate"} {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			cmd := osexec.CommandContext(ctx, p.tools["pg_ctl"], "-D", dir, "-m", mode, "-t", "20", "-w", "stop")
			out, err := cmd.CombinedOutput()
			cancel()
			if err == nil || strings.Contains(string(out), "Is server running?") || strings.Contains(string(out), "does not exist") {
				break
			}
			if mode == "immediate" {
				p.t.Errorf("isolated cluster cleanup failed: %s", fmt.Sprint(err))
			}
		}
	}
}

package e2e_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ashhart/tf-starlight/internal/crypto/keyring"
	"github.com/ashhart/tf-starlight/internal/testutil"
	"github.com/ashhart/tf-starlight/internal/testutil/fakeengine"
)

// takeoverBudget is the docs' "about 15 seconds": the lease, a standby's one-second poll and a second of slack.
const takeoverBudget = 17 * time.Second

var probe = &http.Client{Timeout: 2 * time.Second}

// drill is a real installation: built binaries, separate processes, a TCP balancer before the node listeners.
type drill struct {
	t      *testing.T
	pool   *pgxpool.Pool
	bin    string
	dir    string
	url    string
	master string
	engine string
	lb     *balancer
	mu     sync.Mutex
	procs  []*proc
}

// proc is one started process and the file its output goes to.
type proc struct {
	name string
	cmd  *osexec.Cmd
	log  string
	done chan struct{}
}

// instance is one `starlight serve` with its own listeners.
type instance struct {
	*proc
	public, nodes, metrics string
}

func newDrill(t *testing.T) *drill {
	pool := testutil.DB(t)
	dir := t.TempDir()
	d := &drill{t: t, pool: pool, dir: dir, bin: filepath.Join(dir, "bin"), url: pool.Config().ConnString(), engine: fakeengine.Binary(t),
		master: filepath.Join(dir, "master-key"), lb: newBalancer(t)}
	build := osexec.Command("go", "build", "-o", d.bin+string(os.PathSeparator), "github.com/ashhart/tf-starlight/cmd/starlight",
		"github.com/ashhart/tf-starlight/cmd/starlight-node")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	if err := os.WriteFile(d.master, []byte(keyring.GenerateMasterKey()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.stop)
	return d
}

// env is what `starlight` reads: the database, the master key, and a public URL that validates.
func (d *drill) env(url, master, public string) []string {
	return append(os.Environ(), "STARLIGHT_DATABASE_URL="+url, "STARLIGHT_MASTER_KEY_FILE="+master, "STARLIGHT_PUBLIC_URL=http://"+public,
		"STARLIGHT_NODE_URL=https://"+d.lb.addr)
}

// run is a command that exits: it returns the output and whether it succeeded.
func (d *drill) run(env []string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := osexec.CommandContext(ctx, name, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (d *drill) tool(name string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(d.bin, name)
}

// start launches a long-running process, logging to a file the test prints when it fails.
func (d *drill) start(name string, env []string, bin string, args ...string) *proc {
	d.t.Helper()
	log := filepath.Join(d.dir, name+".log")
	f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		d.t.Fatal(err)
	}
	cmd := osexec.Command(bin, args...)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, f, f
	if err := cmd.Start(); err != nil {
		d.t.Fatal(err)
	}
	p := &proc{name: name, cmd: cmd, log: log, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		f.Close()
		close(p.done)
	}()
	d.mu.Lock()
	d.procs = append(d.procs, p)
	d.mu.Unlock()
	return p
}

func (d *drill) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range d.procs {
		_ = p.cmd.Process.Kill()
		<-p.done
		if d.t.Failed() {
			if b, err := os.ReadFile(p.log); err == nil {
				d.t.Logf("--- %s ---\n%s", p.name, tail(b, 4000))
			}
		}
	}
}

func tail(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}

// serve starts an instance named name against url with the given master key, and waits until it answers.
func (d *drill) serve(name, url, master string) *instance {
	d.t.Helper()
	in := &instance{public: freeAddr(d.t), nodes: freeAddr(d.t), metrics: freeAddr(d.t)}
	env := append(d.env(url, master, in.public), "STARLIGHT_LISTEN_PUBLIC="+in.public, "STARLIGHT_LISTEN_NODES="+in.nodes, "STARLIGHT_LISTEN_METRICS="+in.metrics)
	in.proc = d.start(name, env, d.tool("starlight"), "serve")
	d.waitFor(name+" to answer", 30*time.Second, func() bool {
		resp, err := probe.Get("http://" + in.public + "/readyz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	d.lb.add(in.nodes)
	return in
}

func (d *drill) waitFor(what string, timeout time.Duration, ok func() bool) {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			d.t.Fatalf("waited %s for %s", timeout, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

var epochLine = regexp.MustCompile(`(?m)^starlight_controller_epoch (\S+)$`)

// epoch reads what an instance's metrics say it holds: its fencing epoch, or 0 when it is not the leader.
func (in *instance) epoch() int64 {
	resp, err := probe.Get("http://" + in.metrics + "/metrics")
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return -1
	}
	m := epochLine.FindSubmatch(b)
	if m == nil {
		return -1
	}
	v, err := strconv.ParseFloat(string(m[1]), 64)
	if err != nil {
		return -1
	}
	return int64(v)
}

// enrol runs the command Starlight prints and starts the agent, which reaches Starlight through the balancer.
func (d *drill) enrol(leader *instance) {
	d.t.Helper()
	env := d.env(d.url, d.master, leader.public)
	printed, err := d.run(env, d.tool("starlight"), "nodes", "token", "--pool", "default", "--auto-approve")
	if err != nil {
		d.t.Fatalf("nodes token: %v\n%s", err, printed)
	}
	fields := strings.Fields(printed)
	if len(fields) < 2 || fields[0] != "starlight-node" {
		d.t.Fatalf("Starlight printed %q", printed)
	}
	listen := freeAddr(d.t)
	node := append(os.Environ(), "STARLIGHT_NODE_STATE_DIR="+filepath.Join(d.dir, "node"), "STARLIGHT_NODE_ENGINE="+d.engine,
		"STARLIGHT_NODE_ADVERTISE="+listen, "STARLIGHT_NODE_LISTEN="+listen)
	if out, err := d.run(node, d.tool("starlight-node"), fields[1:]...); err != nil {
		d.t.Fatalf("enrol: %v\n%s", err, out)
	}
	d.start("node", node, d.tool("starlight-node"), "run")
}

// balancer forwards TCP to any listener that answers, as the docs ask of what stands before the node listeners.
type balancer struct {
	addr string
	mu   sync.Mutex
	to   []string
	next int
}

func newBalancer(t *testing.T) *balancer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	b := &balancer{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go b.forward(c)
		}
	}()
	return b
}

func (b *balancer) add(addr string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.to = append(b.to, addr)
}

func (b *balancer) forward(c net.Conn) {
	defer c.Close()
	b.mu.Lock()
	to := append([]string(nil), b.to...)
	first := b.next
	b.next++
	b.mu.Unlock()
	for i := range to {
		up, err := net.DialTimeout("tcp", to[(first+i)%len(to)], 2*time.Second)
		if err != nil {
			continue
		}
		defer up.Close()
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
		go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
		<-done
		return
	}
}

// watchPlacements records each deployment's peak of live replicas, so a duplicate that comes and goes still shows.
func watchPlacements(ctx context.Context, f *fleet) func() map[string]int {
	var mu sync.Mutex
	peak := map[string]int{}
	go func() {
		for ctx.Err() == nil {
			rows, err := f.pool.Query(ctx, `SELECT d.name, count(r.id) FROM deployments d JOIN replicas r ON r.deployment_id = d.id
				WHERE r.desired = 'running' AND r.state NOT IN ('stopped', 'lost', 'failed') GROUP BY d.name`)
			if err == nil {
				for rows.Next() {
					var name string
					var n int
					if rows.Scan(&name, &n) == nil {
						mu.Lock()
						peak[name] = max(peak[name], n)
						mu.Unlock()
					}
				}
				rows.Close()
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	return func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]int, len(peak))
		for k, v := range peak {
			out[k] = v
		}
		return out
	}
}

func (d *drill) chat(f *fleet, in *instance) (int, string) {
	d.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://"+in.public+"/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3.8-27b","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+f.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// kill ends a process at once, as a lost machine would.
func (p *proc) kill() {
	_ = p.cmd.Process.Kill()
	<-p.done
}

// liveReplicas lists the running replicas of a deployment; a duplicate placement shows as a second id.
func (d *drill) liveReplicas(name string) []string {
	d.t.Helper()
	var out []string
	err := d.pool.QueryRow(context.Background(), `SELECT coalesce(array_agg(r.id ORDER BY r.id), '{}') FROM replicas r JOIN deployments d ON d.id = r.deployment_id
		WHERE d.name = $1 AND r.desired = 'running' AND r.state NOT IN ('stopped', 'lost', 'failed')`, name).Scan(&out)
	if err != nil {
		d.t.Fatal(err)
	}
	return out
}

func (d *drill) ready(name string) bool {
	var n int
	_ = d.pool.QueryRow(context.Background(), `SELECT count(*) FROM replicas r JOIN deployments d ON d.id = r.deployment_id
		WHERE d.name = $1 AND r.state = 'ready'`, name).Scan(&n)
	return n > 0
}

func (d *drill) expectOnePlacement(peaks func() map[string]int, names ...string) {
	d.t.Helper()
	peak := peaks()
	for _, name := range names {
		if live := d.liveReplicas(name); len(live) != 1 || peak[name] != 1 {
			d.t.Fatalf("deployment %s has live replicas %v and peaked at %d; it wants one", name, live, peak[name])
		}
	}
}

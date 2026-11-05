//go:build windows

package e2e_test

import (
	"context"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/ashhart/tf-starlight/internal/node/service"
)

var aceTrustee = regexp.MustCompile(`\(A;[^;]*;[^;]*;[^;]*;[^;]*;([^)]+)\)`)

// trustees lists who a path's DACL admits, and whether it refuses to inherit from its parent.
func trustees(t *testing.T, path string) ([]string, bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	sddl := sd.String()
	var out []string
	for _, m := range aceTrustee.FindAllStringSubmatch(sddl, -1) {
		out = append(out, m[1])
	}
	return out, strings.Contains(sddl, "D:P") || strings.Contains(sddl, "D:PAI")
}

// onlySystemAndAdministrators fails on any path under root that admits anyone else; S-1-5-18 is SYSTEM in full.
func onlySystemAndAdministrators(t *testing.T, root string) {
	t.Helper()
	icacls, _ := osexec.Command("icacls", root).CombinedOutput()
	t.Logf("icacls on the state directory:\n%s", icacls)
	if _, protected := trustees(t, root); !protected {
		t.Errorf("%s inherits from its parent", root)
	}
	seen := 0
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		seen++
		who, _ := trustees(t, path)
		for _, w := range who {
			if w != "SY" && w != "BA" && w != "S-1-5-18" {
				t.Errorf("%s also admits %s (%v)", path, w, who)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("checked the DACL of %d paths under the state directory", seen)
}

func waitState(t *testing.T, s *mgr.Service, want svc.State, timeout time.Duration) svc.Status {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st, err := s.Query()
		if err != nil {
			t.Fatal(err)
		}
		if st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("the service stayed in state %d, wanted %d", st.State, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func processAlive(pid int) bool {
	out, err := osexec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH").CombinedOutput()
	return err == nil && strings.Contains(string(out), " "+strconv.Itoa(pid)+" ")
}

// enginePID reads the pid file the agent keeps beside a running engine.
func enginePID(t *testing.T, state string) int {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(state, "replicas", "*", "engine.pid"))
	if len(files) != 1 {
		t.Fatalf("pid files %v", files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0]))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// TestWindowsNodeService runs install-windows.ps1 for real; it needs an elevated runner and STARLIGHT_DRILL=1.
func TestWindowsNodeService(t *testing.T) {
	if os.Getenv("STARLIGHT_DRILL") == "" {
		t.Skip("set STARLIGHT_DRILL=1 to install a real service on this machine")
	}
	d := newDrill(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if out, err := d.run(d.env(d.url, d.master, "127.0.0.1:1"), d.tool("starlight"), "init", "--org", "Example Corp"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	f := &fleet{t: t, ctx: ctx, pool: d.pool}
	f.catalogue()
	in := d.serve("starlight", d.url, d.master)
	d.waitFor("the instance to lead", 30*time.Second, func() bool { return in.epoch() > 0 })
	printed, err := d.run(d.env(d.url, d.master, in.public), d.tool("starlight"), "nodes", "token", "--pool", "default", "--auto-approve")
	if err != nil {
		t.Fatalf("nodes token: %v\n%s", err, printed)
	}
	flags := map[string]string{}
	fields := strings.Fields(printed)
	for i := 2; i+1 < len(fields); i += 2 {
		flags[fields[i]] = fields[i+1]
	}
	state := filepath.Join(os.Getenv("ProgramData"), "TensorFold", "StarlightNode")
	dir := filepath.Join(os.Getenv("ProgramFiles"), "TensorFold", "Starlight Node")
	installed := filepath.Join(dir, "starlight-node.exe")
	addr := freeAddr(t)
	_, port, _ := strings.Cut(addr, ":")
	t.Cleanup(func() {
		if t.Failed() {
			if b, err := os.ReadFile(filepath.Join(state, "starlight-node.log")); err == nil {
				t.Logf("--- the service's log ---\n%s", tail(b, 4000))
			}
		}
		_, _ = d.run(os.Environ(), installed, "service", "uninstall")
		_, _ = d.run(os.Environ(), "powershell", "-NoProfile", "-Command", "Remove-NetFirewallRule -DisplayName 'TensorFold Starlight Node "+port+"'")
		_ = os.RemoveAll(state)
		_ = os.RemoveAll(dir)
	})
	out, err := d.run(os.Environ(), "powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", filepath.Join("..", "..", "deploy", "node", "install-windows.ps1"),
		"-Server", flags["--server"], "-Token", flags["--token"], "-CASha256", flags["--ca-sha256"], "-Engine", d.engine, "-Binary", d.tool("starlight-node"),
		"-Name", "windows-1", "-Port", port)
	t.Logf("install-windows.ps1:\n%s", out)
	if err != nil {
		t.Fatalf("install-windows.ps1: %v", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(service.WindowsName)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := s.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StartType != mgr.StartAutomatic || !strings.EqualFold(cfg.ServiceStartName, "LocalSystem") {
		t.Fatalf("service config %+v", cfg)
	}
	actions, err := s.RecoveryActions()
	if err != nil || len(actions) == 0 || actions[0].Type != mgr.ServiceRestart {
		t.Fatalf("recovery actions %+v, %v", actions, err)
	}
	if rule, err := d.run(os.Environ(), "powershell", "-NoProfile", "-Command", "Get-NetFirewallRule -DisplayName 'TensorFold Starlight Node "+port+"'"); err != nil {
		t.Fatalf("the firewall rule is missing: %v\n%s", err, rule)
	}
	first := waitState(t, s, svc.Running, time.Minute)

	// The service supervises an engine, which serves through the gateway.
	d.waitFor("the replica to become ready under the service", 2*time.Minute, func() bool { return d.ready("qwen-mac") })
	if code, body := d.chat(f, in); code != http.StatusOK {
		t.Fatalf("chat through the service: %d %s", code, body)
	}
	onlySystemAndAdministrators(t, state)

	// An administrator looking at the node leaves the state directory as it was.
	if out, err := d.run(append(os.Environ(), "STARLIGHT_NODE_STATE_DIR="+state), installed, "status"); err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	onlySystemAndAdministrators(t, state)

	// The agent dies: Windows ends its engine with it and restarts the service.
	engine := enginePID(t, state)
	if out, err := d.run(os.Environ(), "taskkill", "/F", "/PID", strconv.Itoa(int(first.ProcessId))); err != nil {
		t.Fatalf("taskkill: %v\n%s", err, out)
	}
	d.waitFor("the engine to die with its agent", 15*time.Second, func() bool { return !processAlive(engine) })
	d.waitFor("the service to restart after a failure", 90*time.Second, func() bool {
		st, err := s.Query()
		return err == nil && st.State == svc.Running && st.ProcessId != first.ProcessId
	})
	d.waitFor("a request to be served again", 2*time.Minute, func() bool {
		code, _ := d.chat(f, in)
		return code == http.StatusOK
	})

	// A clean stop and start, as at shutdown and boot.
	engine = enginePID(t, state)
	if _, err := s.Control(svc.Stop); err != nil {
		t.Fatal(err)
	}
	waitState(t, s, svc.Stopped, 2*time.Minute)
	d.waitFor("the engine to stop with the service", 15*time.Second, func() bool { return !processAlive(engine) })
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	waitState(t, s, svc.Running, time.Minute)
	d.waitFor("a request to be served after a start", 2*time.Minute, func() bool {
		code, _ := d.chat(f, in)
		return code == http.StatusOK
	})
	onlySystemAndAdministrators(t, state)

	// Windows finishes deleting a service only when the last handle to it closes.
	s.Close()
	if out, err := d.run(os.Environ(), installed, "service", "uninstall"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	d.waitFor("the service to be gone", time.Minute, func() bool {
		st, err := m.OpenService(service.WindowsName)
		if err != nil {
			return true
		}
		st.Close()
		return false
	})
	if _, err := os.Stat(filepath.Join(state, "node.json")); err != nil {
		t.Fatalf("uninstall lost the node's state: %v", err)
	}
}

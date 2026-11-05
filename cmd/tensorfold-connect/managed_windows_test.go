package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ashhart/tf-starlight/internal/cli"
	"github.com/ashhart/tf-starlight/internal/connect/profile"
	"github.com/ashhart/tf-starlight/internal/connect/store"
)

// TestShippedPolicyLocksTheServer writes the shipped .reg file as Group Policy would; it needs an elevated runner.
func TestShippedPolicyLocksTheServer(t *testing.T) {
	if os.Getenv("TENSORFOLD_CONNECT_POLICY_TEST") != "1" {
		t.Skip("set TENSORFOLD_CONNECT_POLICY_TEST=1 to write machine policy to this computer's registry")
	}
	reg := filepath.Join("..", "..", "deploy", "mdm", "windows", "tensorfold-connect.reg")
	if out, err := exec.Command("reg", "import", reg).CombinedOutput(); err != nil {
		t.Fatalf("reg import: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("reg", "delete", `HKLM\SOFTWARE\Policies\TensorFold`, "/f").Run() })
	t.Setenv(store.BackendEnv, "file")
	t.Setenv(profile.HomeEnv, t.TempDir())
	var out, errOut bytes.Buffer
	oldOut, oldErr := stdout, stderr
	stdout, stderr = &out, &errOut
	t.Cleanup(func() { stdout, stderr = oldOut, oldErr })
	if err := cli.Dispatch(context.Background(), root(), []string{"status"}, &out, &errOut); err != nil {
		t.Fatalf("status: %v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "https://starlight.example.com (set by your organisation)") {
		t.Fatalf("status does not show the managed server:\n%s", out.String())
	}
	err := cli.Dispatch(context.Background(), root(), []string{"login", "--server", "https://other.example"}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "your organisation manages") {
		t.Fatalf("a person changed the managed server: %v", err)
	}
}

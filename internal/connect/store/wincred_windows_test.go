package store

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestTokensLandInCredentialManager asks Windows itself whether an entry Connect wrote is there.
func TestTokensLandInCredentialManager(t *testing.T) {
	if os.Getenv("TENSORFOLD_CONNECT_PLATFORM_STORE_TEST") != "1" {
		t.Skip("set TENSORFOLD_CONNECT_PLATFORM_STORE_TEST=1 to exercise the real credential store")
	}
	s := Open(t.TempDir())
	if !strings.Contains(s.Backend(), "Credential Manager") {
		t.Fatalf("Connect chose %s", s.Backend())
	}
	if err := s.Set("visible-entry", []byte("value")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Delete("visible-entry") })
	out, err := exec.Command("cmdkey", "/list").CombinedOutput()
	if err != nil || !strings.Contains(string(out), Service+"/visible-entry") {
		t.Fatalf("cmdkey /list does not show the entry (%v):\n%s", err, out)
	}
}

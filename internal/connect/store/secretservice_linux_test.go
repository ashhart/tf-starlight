package store

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestTokensLandInTheSecretService asks secret-tool whether an entry Connect wrote is in the keyring.
func TestTokensLandInTheSecretService(t *testing.T) {
	if os.Getenv("TENSORFOLD_CONNECT_PLATFORM_STORE_TEST") != "1" {
		t.Skip("set TENSORFOLD_CONNECT_PLATFORM_STORE_TEST=1 to exercise the real credential store")
	}
	s := Open(t.TempDir())
	if s.Backend() != "Secret Service" {
		t.Fatalf("Connect chose %s", s.Backend())
	}
	if err := s.Set("visible-entry", []byte("value")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Delete("visible-entry") })
	out, err := exec.Command("secret-tool", "lookup", "application", "tensorfold-connect", "key", "visible-entry").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "value" {
		t.Fatalf("secret-tool lookup = %q, %v", out, err)
	}
}

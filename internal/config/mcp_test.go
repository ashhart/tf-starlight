package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ashhart/tf-starlight/internal/ids"
)

func validMCP() MCP {
	return MCP{Enabled: true, Endpoint: "https://tools.example.com/mcp", ProjectID: ids.New(), KeyIDs: []string{ids.New()},
		Tools: []string{"echo"}, MaxClass: "internal", MaxConcurrent: 2, Timeout: Duration{Duration: 10 * time.Second}}
}

func TestMCPIsExplicitAndBounded(t *testing.T) {
	if Defaults().MCP.Enabled {
		t.Fatal("tool authority enabled by default")
	}
	if err := validMCP().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*MCP){
		func(m *MCP) { m.Endpoint = "http://tools.example.com/mcp" },
		func(m *MCP) { m.Endpoint = "https://user:fixture@tools.example.com/mcp" },
		func(m *MCP) { m.Endpoint += "?key=fixture" },
		func(m *MCP) { m.ProjectID = "" }, func(m *MCP) { m.KeyIDs = nil }, func(m *MCP) { m.KeyIDs = append(m.KeyIDs, m.KeyIDs[0]) },
		func(m *MCP) { m.Tools = nil }, func(m *MCP) { m.MaxClass = "" }, func(m *MCP) { m.MaxConcurrent = 0 },
		func(m *MCP) { m.MaxConcurrent = 65 }, func(m *MCP) { m.Timeout.Duration = 2 * time.Minute },
		func(m *MCP) { m.BearerEnv = "MCP_FIXTURE"; m.BearerFile = "fixture" },
	} {
		m := validMCP()
		edit(&m)
		if m.Validate() == nil {
			t.Fatal("partial or unbounded grant accepted")
		}
	}
}

func TestMCPCredentialSourceIsSeparateAndBounded(t *testing.T) {
	m := validMCP()
	m.BearerEnv = "STARLIGHT_MCP_FIXTURE"
	t.Setenv(m.BearerEnv, "")
	if _, err := m.Bearer(); err == nil {
		t.Fatal("missing explicit credential accepted")
	}
	t.Setenv(m.BearerEnv, "fixture\nInjected: yes")
	if _, err := m.Bearer(); err == nil {
		t.Fatal("header injection accepted")
	}
	m.BearerEnv = ""
	m.BearerFile = filepath.Join(t.TempDir(), "upstream-secret")
	if err := os.WriteFile(m.BearerFile, []byte("fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := m.Bearer(); err != nil || got != "fixture" {
		t.Fatal("file credential failed")
	}
	if err := os.WriteFile(m.BearerFile, make([]byte, 8193), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Bearer(); err == nil {
		t.Fatal("unbounded credential file read")
	}
}

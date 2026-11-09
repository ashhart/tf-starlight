package config

import (
	"errors"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ashhart/tf-starlight/internal/ids"
	"github.com/ashhart/tf-starlight/internal/levels"
)

// MCP grants one project's explicit keys access to one fixed tool server; it is disabled by default.
type MCP struct {
	Enabled       bool     `yaml:"enabled"`
	Endpoint      string   `yaml:"endpoint"`
	Internal      bool     `yaml:"internal"`
	ProjectID     string   `yaml:"project_id"`
	KeyIDs        []string `yaml:"key_ids"`
	Tools         []string `yaml:"tools"`
	MaxClass      string   `yaml:"max_class"`
	BearerEnv     string   `yaml:"bearer_env"`
	BearerFile    string   `yaml:"bearer_file"`
	MaxConcurrent int      `yaml:"max_concurrent"`
	Timeout       Duration `yaml:"timeout"`
}

// Validate refuses partial grants and unsafe resource bounds even before constructing the HTTP client.
func (m MCP) Validate() error {
	if !m.Enabled {
		return nil
	}
	u, err := url.Parse(m.Endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && !(m.Internal && u.Scheme == "http")) {
		return errors.New("mcp.endpoint must be an approved absolute HTTP URL without credentials or query")
	}
	if !ids.Valid(m.ProjectID) || len(m.KeyIDs) == 0 || len(m.KeyIDs) > 128 || len(m.Tools) == 0 || len(m.Tools) > 128 {
		return errors.New("mcp requires a project id and bounded explicit key_ids and tools")
	}
	seen := map[string]bool{}
	for _, id := range m.KeyIDs {
		if !ids.Valid(id) || seen[id] {
			return errors.New("mcp.key_ids must contain unique valid ids")
		}
		seen[id] = true
	}
	seen = map[string]bool{}
	for _, name := range m.Tools {
		if name == "" || len(name) > 128 || strings.TrimSpace(name) != name || strings.ContainsAny(name, "\r\n\x00") || seen[name] {
			return errors.New("mcp.tools must contain unique bounded names")
		}
		seen[name] = true
	}
	if _, err := levels.ParseClass(m.MaxClass); err != nil {
		return errors.New("mcp.max_class must explicitly bound the upstream's classification")
	}
	if m.BearerEnv != "" && m.BearerFile != "" {
		return errors.New("mcp accepts one of bearer_env and bearer_file")
	}
	if m.MaxConcurrent < 1 || m.MaxConcurrent > 64 || m.Timeout.Duration < time.Second || m.Timeout.Duration > time.Minute {
		return errors.New("mcp requires max_concurrent between 1 and 64 and timeout between 1s and 1m")
	}
	return nil
}

// Bearer reads only the separately configured upstream secret, without putting its value in diagnostics.
func (m MCP) Bearer() (string, error) {
	var value string
	if m.BearerEnv != "" {
		value = os.Getenv(m.BearerEnv)
	}
	if m.BearerFile != "" {
		file, err := os.Open(m.BearerFile)
		if err != nil {
			return "", errors.New("cannot read MCP upstream credential file")
		}
		defer file.Close()
		raw, err := io.ReadAll(io.LimitReader(file, 8193))
		if err != nil || len(raw) > 8192 {
			return "", errors.New("MCP upstream credential exceeds its bound or cannot be read")
		}
		value = strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	}
	if len(value) > 8192 || strings.ContainsAny(value, "\r\n\x00") || strings.TrimSpace(value) != value ||
		(m.BearerEnv != "" || m.BearerFile != "") && value == "" {
		return "", errors.New("MCP upstream credential is missing or malformed")
	}
	return value, nil
}

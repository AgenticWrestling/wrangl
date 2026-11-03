// Package runner implements runner mode: bootstrap (enrol once), the daemon and status.
package runner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Defaults written by bootstrap.
const (
	DefaultConfigPath = "/etc/wrangl/runner.yaml"
	DefaultStateDir   = "/var/lib/wrangl"
	DefaultAgentUser  = "agent"
	DefaultMiseBin    = "/usr/local/bin/mise"
	DefaultSocketPath = "/run/wrangl/runner.sock"
)

// Config is /etc/wrangl/runner.yaml, written by bootstrap.
type Config struct {
	RunnerID   int64    `yaml:"runner_id"`
	Name       string   `yaml:"name"`
	Controller string   `yaml:"controller"` // enrol listener URL
	NATSURLs   []string `yaml:"nats_urls"`
	CAFile     string   `yaml:"ca_file"`
	CredsFile  string   `yaml:"creds_file"`
	// PoweroffCommand runs when the controller has been unreachable for OrphanPoweroff.
	PoweroffCommand []string `yaml:"poweroff_command"`
	// StateDir holds the applied template version and setup-script hash (root only).
	StateDir string `yaml:"state_dir"`
	// AgentUser runs the agent and owns its toolchain. Empty = the daemon's own user
	// (tests and non-root development only).
	AgentUser string `yaml:"agent_user"`
	// AgentHome overrides the agent's home directory (default: from the passwd entry).
	AgentHome string `yaml:"agent_home,omitempty"`
	MiseBin   string `yaml:"mise_bin"`
	// Host marks the controller's own machine enrolled as a permanent runner: it runs as the
	// current user in that user's real home, never powers the box off, and leaves the user's
	// mise config and tools alone (templates are acknowledged, not applied).
	Host bool `yaml:"host,omitempty"`
	// SocketPath is the daemon's local socket for the agent's hooks and MCP shim.
	SocketPath string `yaml:"socket_path,omitempty"`
}

// Socket returns the agent socket path.
func (c *Config) Socket() string {
	if c.SocketPath != "" {
		return c.SocketPath
	}
	return DefaultSocketPath
}

// LoadConfig reads a runner config.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read runner config (has this box been bootstrapped?): %w", err)
	}
	c := Config{StateDir: DefaultStateDir, MiseBin: DefaultMiseBin}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.RunnerID <= 0 || len(c.NATSURLs) == 0 || c.CAFile == "" || c.CredsFile == "" {
		return nil, fmt.Errorf("%s is incomplete", path)
	}
	return &c, nil
}

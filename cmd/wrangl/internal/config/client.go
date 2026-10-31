package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// CFServiceToken is a Cloudflare Access service token for non-browser clients.
type CFServiceToken struct {
	ID     string `yaml:"id" json:"id"`
	Secret string `yaml:"secret" json:"secret"`
}

// Client is ~/.config/wrangl/config.yaml.
type Client struct {
	Controller     string          `yaml:"controller" json:"controller"`
	CFServiceToken *CFServiceToken `yaml:"cf_service_token,omitempty" json:"cf_service_token,omitempty"`
}

// DefaultLocalController is the unix socket used on the controller host itself.
const DefaultLocalController = "unix:///run/wrangl/api.sock"

// DefaultClientPath returns $XDG_CONFIG_HOME/wrangl/config.yaml.
func DefaultClientPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "wrangl", "config.yaml"), nil
}

// LoadClient reads path; a missing file yields defaults.
func LoadClient(path string) (*Client, error) {
	c := &Client{Controller: DefaultLocalController}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read client config: %w", err)
	}
	if err := decodeStrict(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Controller == "" {
		c.Controller = DefaultLocalController
	}
	return c, nil
}

// SaveClient writes c to path (0600: it may hold a service token secret).
func SaveClient(path string, c *Client) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// Redacted returns a copy safe to print.
func (c *Client) Redacted() *Client {
	r := *c
	if c.CFServiceToken != nil {
		t := *c.CFServiceToken
		if t.Secret != "" {
			t.Secret = "REDACTED"
		}
		r.CFServiceToken = &t
	}
	return &r
}

// Resolve applies precedence flag > env > file > default for a single string setting.
func Resolve(flag, envKey, file, def string) string {
	switch {
	case flag != "":
		return flag
	case os.Getenv(envKey) != "":
		return os.Getenv(envKey)
	case file != "":
		return file
	}
	return def
}

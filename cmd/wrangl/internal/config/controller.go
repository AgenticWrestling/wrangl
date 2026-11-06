// Package config loads the controller and client configuration files.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Listen holds the controller's listen addresses.
type Listen struct {
	HTTP   string `yaml:"http"`
	Unix   string `yaml:"unix"`
	NATS   string `yaml:"nats"`
	Enroll string `yaml:"enroll"`
}

// AWS holds where the controller launches runners.
type AWS struct {
	Region                string   `yaml:"region"`
	SubnetIDs             []string `yaml:"subnet_ids"`
	SecurityGroupID       string   `yaml:"security_group_id"`
	RunnerInstanceProfile string   `yaml:"runner_instance_profile"`
}

// GitHubApp identifies one GitHub App. Key paths are relative to data_dir.
type GitHubApp struct {
	ID            int64  `yaml:"id"`
	PrivateKey    string `yaml:"private_key"`
	WebhookSecret string `yaml:"webhook_secret,omitempty"` // legacy reviewer app only
}

// GitHub holds the legacy single-App config. Per-account Apps now live in the database (one App
// per account; an optional reviewer App for repos that require approving reviews).
type GitHub struct {
	AuthorApp   GitHubApp `yaml:"author_app"`
	ReviewerApp GitHubApp `yaml:"reviewer_app"`
	// PATFile holds a personal access token, used for any owner that has no App. Relative
	// paths resolve under data_dir, like the App keys. It is a fallback and not an override:
	// an account that has an App keeps using it, and its per-repository hour-long tokens.
	// See controller/gh/pat.go for what this mode cannot do.
	PATFile string `yaml:"pat_file,omitempty"`
}

// Cloudflare holds the Access application used to verify requests.
type Cloudflare struct {
	Team       string `yaml:"team"`
	AUD        string `yaml:"aud"`
	OwnerEmail string `yaml:"owner_email"`
}

// Web holds the public URL the PWA is served on.
type Web struct {
	PublicURL string `yaml:"public_url"`
}

// Controller is /etc/wrangl/controller.yaml.
type Controller struct {
	DataDir    string     `yaml:"data_dir"`
	Listen     Listen     `yaml:"listen"`
	AWS        AWS        `yaml:"aws"`
	GitHub     GitHub     `yaml:"github"`
	Cloudflare Cloudflare `yaml:"cloudflare"`
	Web        Web        `yaml:"web"`
}

// Default controller paths and addresses.
const (
	DefaultControllerConfig = "/etc/wrangl/controller.yaml"
	DefaultDataDir          = "/var/lib/wrangl"
)

// DefaultController returns the controller defaults. NATS and the enrol listener bind
// loopback by default; production configs set the controller's private VPC address.
func DefaultController() *Controller {
	return &Controller{
		DataDir: DefaultDataDir,
		Listen: Listen{
			HTTP:   "127.0.0.1:8080",
			Unix:   "/run/wrangl/api.sock",
			NATS:   "127.0.0.1:4222",
			Enroll: "127.0.0.1:4223",
		},
	}
}

// LoadController reads path over the defaults. Unknown keys are rejected so a typo
// cannot silently fall back to a default.
func LoadController(path string) (*Controller, error) {
	c := DefaultController()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read controller config: %w", err)
	}
	if err := decodeStrict(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.DataDir == "" {
		return nil, fmt.Errorf("%s: data_dir must not be empty", path)
	}
	return c, nil
}

// Path resolves rel against DataDir; absolute paths are returned unchanged.
func (c *Controller) Path(rel string) string {
	if rel == "" || filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(c.DataDir, rel)
}

// Marshal renders the config as YAML.
func (c *Controller) Marshal() ([]byte, error) { return yaml.Marshal(c) }

func decodeStrict(b []byte, v any) error {
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

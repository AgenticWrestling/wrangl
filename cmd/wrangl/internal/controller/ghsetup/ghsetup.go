// Package ghsetup creates the GitHub author App through GitHub's App manifest flow, so
// the owner can set it up from the web app instead of by hand. The flow:
//
//  1. The Setup page POSTs a manifest to github.com; the owner confirms there.
//  2. GitHub redirects the owner's browser to the callback with a one-time code.
//  3. Exchange swaps the code for the App's id and private key and stores them in the
//     data directory, where the controller picks them up at start (see Load).
package ghsetup

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// KeyRel is where the author App's private key is stored, relative to the data dir.
const KeyRel = "secrets/gh-author.pem"

const metaFile = "github-author.json"

// App is what the setup flow records about the created App (the key is a separate file).
type App struct {
	ID      int64  `json:"id"`
	Slug    string `json:"slug"`
	HTMLURL string `json:"html_url"`
}

// InstallURL is where the owner installs the App on their repositories.
func (a App) InstallURL() string { return "https://github.com/apps/" + a.Slug + "/installations/new" }

// Service holds the setup state for one controller.
type Service struct {
	DataDir   string
	PublicURL string       // https://ctrl.example.com, no trailing slash
	AppName   string       // suggested App name (GitHub App names are global)
	API       string       // GitHub API base; default https://api.github.com
	HTTP      *http.Client // default: 20s timeout
	Now       func() time.Time

	mu     sync.Mutex
	states map[string]time.Time
}

const stateTTL = time.Hour

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Manifest is the App manifest to submit to GitHub for an App called name.
func (s *Service) Manifest(name string) ([]byte, error) {
	if s.PublicURL == "" {
		return nil, errors.New("web.public_url is not set in controller.yaml")
	}
	return json.Marshal(map[string]any{
		"name": name, "url": s.PublicURL, "public": false,
		"redirect_url":    s.PublicURL + "/setup/github/callback",
		"hook_attributes": map[string]any{"url": s.PublicURL + "/gh/webhook", "active": false},
		"default_permissions": map[string]string{
			"contents": "write", "pull_requests": "write", "issues": "read", "metadata": "read",
		},
		"default_events": []string{},
	})
}

// NewState returns a single-use token for one trip to GitHub and back.
func (s *Service) NewState() string {
	b := make([]byte, 24)
	rand.Read(b)
	st := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = map[string]time.Time{}
	}
	for k, exp := range s.states {
		if s.now().After(exp) {
			delete(s.states, k)
		}
	}
	s.states[st] = s.now().Add(stateTTL)
	return st
}

func (s *Service) takeState(st string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.states[st]
	delete(s.states, st)
	return ok && !s.now().After(exp)
}

// Exchange completes the flow: it checks state, trades code for the App's credentials and
// stores them. The code is valid for an hour and works once.
func (s *Service) Exchange(ctx context.Context, code, state string) (App, error) {
	if !s.takeState(state) {
		return App{}, errors.New("this setup link has expired or was already used; start again from Setup")
	}
	if code == "" {
		return App{}, errors.New("GitHub sent no code")
	}
	api := s.API
	if api == "" {
		api = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", api+"/app-manifests/"+code+"/conversions", nil)
	if err != nil {
		return App{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "wrangl")
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return App{}, fmt.Errorf("GitHub: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return App{}, fmt.Errorf("GitHub refused the code (%d): %.200s", resp.StatusCode, body)
	}
	var d struct {
		App
		PEM string `json:"pem"`
	}
	if err := json.Unmarshal(body, &d); err != nil || d.ID == 0 || d.PEM == "" {
		return App{}, errors.New("GitHub's reply had no App id or key")
	}
	if err := s.store(d.App, d.PEM); err != nil {
		return App{}, err
	}
	return d.App, nil
}

func (s *Service) store(a App, pem string) error {
	key := filepath.Join(s.DataDir, KeyRel)
	if err := os.MkdirAll(filepath.Dir(key), 0o750); err != nil {
		return err
	}
	if err := writeFile(key, []byte(pem)); err != nil {
		return err
	}
	meta, _ := json.Marshal(a)
	return writeFile(filepath.Join(s.DataDir, metaFile), meta)
}

// writeFile replaces path atomically with mode 0600.
func writeFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Current returns the App this service created, if any.
func (s *Service) Current() (App, bool) { return Load(s.DataDir) }

// Load returns the App the setup flow created, if any.
func Load(dataDir string) (App, bool) {
	b, err := os.ReadFile(filepath.Join(dataDir, metaFile))
	if err != nil {
		return App{}, false
	}
	var a App
	if json.Unmarshal(b, &a) != nil || a.ID == 0 {
		return App{}, false
	}
	return a, true
}

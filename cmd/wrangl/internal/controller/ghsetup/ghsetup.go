// Package ghsetup creates GitHub author Apps through GitHub's App manifest flow, so the owner
// can set them up from the web app instead of by hand.
//
// A GitHub App is created in, and can by default only be installed on, one account: the
// owner's personal account or an organisation. wrangl therefore keeps a list of GitHub
// accounts (Settings) and one author App per account. The flow for an account:
//
//  1. Begin: the Settings page POSTs a manifest to github.com (the personal or the
//     organisation form of "new App"); the owner confirms there.
//  2. GitHub redirects the owner's browser to the callback with a one-time code.
//  3. Exchange swaps the code for the App's id and private key, stores the key in the data
//     directory and records the App on the account.
package ghsetup

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
)

// KeyRel is where the legacy single author App's private key was stored (relative to the data
// dir); MigrateLegacy adopts it into an account.
const KeyRel = "secrets/gh-author.pem"

const metaFile = "github-author.json"

// App is what GitHub reports about a created App (the key is a separate file).
type App struct {
	ID      int64  `json:"id"`
	Slug    string `json:"slug"`
	HTMLURL string `json:"html_url"`
}

// InstallURL is where the owner installs the App on their repositories.
func (a App) InstallURL() string { return "https://github.com/apps/" + a.Slug + "/installations/new" }

// Account is a GitHub account wrangl works with, and its App if one exists.
type Account struct {
	Login string
	Kind  string // user | org
	App   *App
	// Reviewer is the second App, which approves and merges pull requests (nil until created).
	Reviewer *App
}

// ReviewerInstallationsURL is like InstallationsURL for the reviewer App.
func (a Account) ReviewerInstallationsURL() string {
	if a.Reviewer == nil {
		return ""
	}
	return a.InstallationsURL2()
}

// InstallationsURL2 is the account's installations page whether or not the author App exists.
func (a Account) InstallationsURL2() string {
	if a.Kind == "org" {
		return "https://github.com/organizations/" + url.PathEscape(a.Login) + "/settings/installations"
	}
	return "https://github.com/settings/installations"
}

// NewAppURL is where an App is created for this account: the personal and organisation forms
// differ, and only an owner of the organisation can use the latter.
func (a Account) NewAppURL(state string) string {
	if a.Kind == "org" {
		return "https://github.com/organizations/" + url.PathEscape(a.Login) + "/settings/apps/new?state=" + url.QueryEscape(state)
	}
	return "https://github.com/settings/apps/new?state=" + url.QueryEscape(state)
}

// AppSettingsURL is where the owner manages the App's installations and repository access.
func (a Account) InstallationsURL() string {
	if a.App == nil {
		return ""
	}
	if a.Kind == "org" {
		return "https://github.com/organizations/" + url.PathEscape(a.Login) + "/settings/installations"
	}
	return "https://github.com/settings/installations"
}

// Service holds the setup state for one controller.
type Service struct {
	DB        *db.DB
	DataDir   string
	PublicURL string       // https://ctrl.example.com, no trailing slash
	API       string       // GitHub API base; default https://api.github.com
	HTTP      *http.Client // default: 20s timeout
	Now       func() time.Time
	// Changed is called after an App is created or an account changes (drop cached clients).
	Changed func()

	mu     sync.Mutex
	states map[string]pending
}

type pending struct {
	login string
	role  string // "" (author) or "reviewer"
	back  string // where to return the owner afterwards (a path on this site)
	exp   time.Time
}

// Result is what a completed flow reports.
type Result struct {
	App   App
	Login string
	Back  string
	Role  string
}

// SafeBack accepts only a path on this site, so the callback cannot be used as an open redirect.
func SafeBack(p string) string {
	if strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.ContainsAny(p, "\\\r\n") {
		return p
	}
	return ""
}

const stateTTL = time.Hour

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) changed() {
	if s.Changed != nil {
		s.Changed()
	}
}

var loginRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// AddAccount links a GitHub account (kind user or org) so repositories under it can be linked.
func (s *Service) AddAccount(ctx context.Context, login, kind string) (Account, error) {
	login = strings.TrimSpace(strings.TrimPrefix(login, "@"))
	if !loginRe.MatchString(login) {
		return Account{}, fmt.Errorf("%q is not a GitHub user or organisation name", login)
	}
	if kind != "user" && kind != "org" {
		return Account{}, errors.New("kind must be user or org")
	}
	row, err := s.DB.Q.UpsertGithubAccount(ctx, dbgen.UpsertGithubAccountParams{Login: login, Kind: kind})
	if err != nil {
		return Account{}, err
	}
	s.changed()
	return toAccount(row), nil
}

// RemoveAccount unlinks an account. Repositories already linked keep working until their App
// is gone; the App itself is not deleted from GitHub.
func (s *Service) RemoveAccount(ctx context.Context, login string) error {
	if err := s.DB.Q.DeleteGithubAccount(ctx, login); err != nil {
		return err
	}
	s.changed()
	return nil
}

func toAccount(r dbgen.GithubAccount) Account {
	a := Account{Login: r.Login, Kind: r.Kind}
	if r.AppID.Valid && r.AppID.Int64 != 0 {
		a.App = &App{ID: r.AppID.Int64, Slug: r.Slug, HTMLURL: r.HtmlUrl}
	}
	if r.ReviewerAppID.Valid && r.ReviewerAppID.Int64 != 0 {
		a.Reviewer = &App{ID: r.ReviewerAppID.Int64, Slug: r.ReviewerSlug, HTMLURL: r.ReviewerHtmlUrl}
	}
	return a
}

// Accounts lists the linked accounts, users first.
func (s *Service) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.DB.RQ.ListGithubAccounts(ctx)
	if err != nil {
		return nil, err
	}
	var out []Account
	for _, r := range rows {
		out = append(out, toAccount(r))
	}
	return out, nil
}

// Account returns one linked account (login is case-insensitive).
func (s *Service) Account(ctx context.Context, login string) (Account, bool) {
	r, err := s.DB.RQ.GetGithubAccount(ctx, login)
	if err != nil {
		return Account{}, false
	}
	return toAccount(r), true
}

// Lookup is gh.Multi's way to find an owner's App.
func (s *Service) Lookup(owner string) (gh.AppRef, bool) {
	r, err := s.DB.RQ.GetGithubAccount(context.Background(), owner)
	if err != nil || !r.AppID.Valid || r.AppID.Int64 == 0 || r.KeyFile == "" {
		return gh.AppRef{}, false
	}
	return gh.AppRef{AppID: r.AppID.Int64, KeyPath: filepath.Join(s.DataDir, r.KeyFile)}, true
}

// LookupReviewer is gh.Multi's way to find an owner's reviewer App.
func (s *Service) LookupReviewer(owner string) (gh.AppRef, bool) {
	r, err := s.DB.RQ.GetGithubAccount(context.Background(), owner)
	if err != nil || !r.ReviewerAppID.Valid || r.ReviewerAppID.Int64 == 0 || r.ReviewerKeyFile == "" {
		return gh.AppRef{}, false
	}
	return gh.AppRef{AppID: r.ReviewerAppID.Int64, KeyPath: filepath.Join(s.DataDir, r.ReviewerKeyFile)}, true
}

// WebhookSecrets are the secrets of every reviewer App (GitHub generates one per App); a
// delivery is accepted if it verifies against any.
func (s *Service) WebhookSecrets() [][]byte {
	rows, err := s.DB.RQ.ListGithubAccounts(context.Background())
	if err != nil {
		return nil
	}
	var out [][]byte
	for _, r := range rows {
		if r.ReviewerHookFile == "" {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(s.DataDir, r.ReviewerHookFile)); err == nil {
			if t := strings.TrimSpace(string(b)); t != "" {
				out = append(out, []byte(t))
			}
		}
	}
	return out
}

// ReviewerName suggests a name for an account's reviewer App.
func ReviewerName(login string) string {
	n := "wrangl-reviewer-" + strings.ToLower(login)
	if len(n) > 34 {
		n = n[:34]
	}
	return strings.TrimRight(n, "-")
}

// AppName suggests a name for an account's App. GitHub App names are global and at most 34
// characters.
func AppName(login string) string {
	n := "wrangl-" + strings.ToLower(login)
	if len(n) > 34 {
		n = n[:34]
	}
	return strings.TrimRight(n, "-")
}

// Manifest is the App manifest to submit to GitHub for an App called name.
func (s *Service) Manifest(name string) ([]byte, error) { return s.ManifestFor(name, "") }

// ManifestFor is the manifest for an author App (role "") or a reviewer App ("reviewer"). The
// reviewer reads checks, approves and merges, and gets webhooks; GitHub generates its secret.
func (s *Service) ManifestFor(name, role string) ([]byte, error) {
	if s.PublicURL == "" {
		return nil, errors.New("web.public_url is not set in controller.yaml")
	}
	if role == "reviewer" {
		return json.Marshal(map[string]any{
			"name": name, "url": s.PublicURL, "public": false,
			"redirect_url":    s.PublicURL + "/setup/github/callback",
			"hook_attributes": map[string]any{"url": s.PublicURL + "/gh/webhook", "active": true},
			"default_permissions": map[string]string{
				"contents": "write", "pull_requests": "write", "checks": "read", "issues": "read", "metadata": "read",
			},
			"default_events": []string{"check_run", "check_suite", "pull_request", "pull_request_review", "issues"},
		})
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

// NewState returns a single-use token for one trip to GitHub and back, for an account; back is
// where the owner returns afterwards (a path on this site, or "").
func (s *Service) NewState(login, back string) string { return s.NewRoleState(login, back, "") }

// NewRoleState is NewState for the author App (role "") or the reviewer App ("reviewer").
func (s *Service) NewRoleState(login, back, role string) string {
	b := make([]byte, 24)
	rand.Read(b)
	st := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = map[string]pending{}
	}
	for k, p := range s.states {
		if s.now().After(p.exp) {
			delete(s.states, k)
		}
	}
	s.states[st] = pending{login: login, role: role, back: SafeBack(back), exp: s.now().Add(stateTTL)}
	return st
}

func (s *Service) takeState(st string) (pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.states[st]
	delete(s.states, st)
	return p, ok && !s.now().After(p.exp)
}

// Exchange completes the flow: it checks state, trades code for the App's credentials, stores
// the key and records the App on the account it was created for. The code is valid for an hour
// and works once. It returns the App, the account's login and where to send the owner next.
func (s *Service) Exchange(ctx context.Context, code, state string) (Result, error) {
	pend, ok := s.takeState(state)
	login := pend.login
	if !ok {
		return Result{}, errors.New("this setup link has expired or was already used; start again from Settings")
	}
	if code == "" {
		return Result{}, errors.New("GitHub sent no code")
	}
	api := s.API
	if api == "" {
		api = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", api+"/app-manifests/"+url.PathEscape(code)+"/conversions", nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "wrangl")
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("GitHub: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return Result{}, fmt.Errorf("GitHub refused the code (%d): %.200s", resp.StatusCode, body)
	}
	var d struct {
		App
		PEM           string `json:"pem"`
		WebhookSecret string `json:"webhook_secret"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	if err := json.Unmarshal(body, &d); err != nil || d.ID == 0 || d.PEM == "" {
		return Result{}, errors.New("GitHub's reply had no App id or key")
	}
	if d.Owner.Login != "" && !strings.EqualFold(d.Owner.Login, login) {
		// Created under a different account than the one we sent the owner to: record it there.
		login = d.Owner.Login
	}
	if pend.role == "reviewer" {
		if err := s.storeReviewer(ctx, login, d.App, d.PEM, d.WebhookSecret); err != nil {
			return Result{}, err
		}
		return Result{App: d.App, Login: login, Back: pend.back, Role: "reviewer"}, nil
	}
	if err := s.store(ctx, login, d.App, d.PEM); err != nil {
		return Result{}, err
	}
	return Result{App: d.App, Login: login, Back: pend.back}, nil
}

func (s *Service) store(ctx context.Context, login string, a App, pem string) error {
	rel := "secrets/gh-" + strings.ToLower(login) + ".pem"
	key := filepath.Join(s.DataDir, rel)
	if err := os.MkdirAll(filepath.Dir(key), 0o750); err != nil {
		return err
	}
	if err := writeFile(key, []byte(pem)); err != nil {
		return err
	}
	// The account may be new (created under another login than the one we expected).
	if _, err := s.DB.RQ.GetGithubAccount(ctx, login); err != nil {
		if _, err := s.DB.Q.UpsertGithubAccount(ctx, dbgen.UpsertGithubAccountParams{Login: login, Kind: "user"}); err != nil {
			return err
		}
	}
	err := s.DB.Q.SetGithubAccountApp(ctx, dbgen.SetGithubAccountAppParams{
		AppID: sql.NullInt64{Int64: a.ID, Valid: true}, Slug: a.Slug, HtmlUrl: a.HTMLURL, KeyFile: rel, Login: login})
	if err == nil {
		s.changed()
	}
	return err
}

func (s *Service) storeReviewer(ctx context.Context, login string, a App, pem, hook string) error {
	rel := "secrets/gh-" + strings.ToLower(login) + "-reviewer.pem"
	hrel := "secrets/gh-" + strings.ToLower(login) + "-reviewer-webhook"
	if err := os.MkdirAll(filepath.Join(s.DataDir, "secrets"), 0o750); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(s.DataDir, rel), []byte(pem)); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(s.DataDir, hrel), []byte(hook)); err != nil {
		return err
	}
	if _, err := s.DB.RQ.GetGithubAccount(ctx, login); err != nil {
		if _, err := s.DB.Q.UpsertGithubAccount(ctx, dbgen.UpsertGithubAccountParams{Login: login, Kind: "user"}); err != nil {
			return err
		}
	}
	err := s.DB.Q.SetGithubAccountReviewer(ctx, dbgen.SetGithubAccountReviewerParams{
		ReviewerAppID: sql.NullInt64{Int64: a.ID, Valid: true}, ReviewerSlug: a.Slug, ReviewerHtmlUrl: a.HTMLURL,
		ReviewerKeyFile: rel, ReviewerHookFile: hrel, Login: login})
	if err == nil {
		s.changed()
	}
	return err
}

// writeFile replaces path atomically with mode 0600.
func writeFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load returns the single App the legacy setup flow created, if any.
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

// MigrateLegacy adopts the old single author App (from the previous Setup page, or an App set in
// controller.yaml: pass its id and key path) into an account, found by asking GitHub who owns
// it. It does nothing when no account has an App yet and none can be read, so it is safe to run
// at every start.
func (s *Service) MigrateLegacy(ctx context.Context, cfgAppID int64, cfgKeyPath string) error {
	accts, err := s.Accounts(ctx)
	if err != nil {
		return err
	}
	for _, a := range accts {
		if a.App != nil {
			return nil // already migrated, or set up fresh
		}
	}
	appID, keyPath, slug, html := int64(0), "", "", ""
	if a, ok := Load(s.DataDir); ok {
		appID, keyPath, slug, html = a.ID, filepath.Join(s.DataDir, KeyRel), a.Slug, a.HTMLURL
	} else if cfgAppID != 0 {
		appID, keyPath = cfgAppID, cfgKeyPath
	}
	if appID == 0 {
		return nil
	}
	key, err := gh.LoadKey(keyPath)
	if err != nil {
		return fmt.Errorf("legacy GitHub App key: %w", err)
	}
	c := &gh.Client{AppID: appID, Key: key, BaseURL: s.API}
	login, kind, err := c.AppOwner(ctx)
	if err != nil {
		return fmt.Errorf("ask GitHub who owns App %d: %w", appID, err)
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(keyPath, s.DataDir), "/")
	if filepath.IsAbs(keyPath) && !strings.HasPrefix(keyPath, s.DataDir) {
		// An App configured with a key outside the data dir: copy it in so the account owns it.
		b, rerr := os.ReadFile(keyPath)
		if rerr != nil {
			return rerr
		}
		rel = "secrets/gh-" + strings.ToLower(login) + ".pem"
		if err := writeFile(filepath.Join(s.DataDir, rel), b); err != nil {
			return err
		}
	}
	if _, err := s.DB.Q.UpsertGithubAccount(ctx, dbgen.UpsertGithubAccountParams{Login: login, Kind: kind}); err != nil {
		return err
	}
	if slug == "" {
		slug = strings.ToLower(login)
	}
	return s.DB.Q.SetGithubAccountApp(ctx, dbgen.SetGithubAccountAppParams{
		AppID: sql.NullInt64{Int64: appID, Valid: true}, Slug: slug, HtmlUrl: html, KeyFile: rel, Login: login})
}

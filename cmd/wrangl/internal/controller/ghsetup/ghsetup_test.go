package ghsetup

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
)

func newSvc(t *testing.T, api string) *Service {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return &Service{DB: d, DataDir: t.TempDir(), PublicURL: "https://ctrl.example.com", API: api}
}

func pemKey(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

func TestAccountsAndAppURLs(t *testing.T) {
	s := newSvc(t, "")
	ctx := context.Background()
	for _, bad := range []string{"", "has space", "-lead", "a/b", strings.Repeat("x", 40)} {
		if _, err := s.AddAccount(ctx, bad, "org"); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := s.AddAccount(ctx, "acme", "team"); err == nil {
		t.Error("accepted kind team")
	}
	me, _ := s.AddAccount(ctx, "njreid", "user")
	org, _ := s.AddAccount(ctx, "@plank-pattern", "org")
	if org.Login != "plank-pattern" {
		t.Fatalf("login %q", org.Login)
	}
	if u := me.NewAppURL("st"); u != "https://github.com/settings/apps/new?state=st" {
		t.Fatal(u)
	}
	if u := org.NewAppURL("st"); u != "https://github.com/organizations/plank-pattern/settings/apps/new?state=st" {
		t.Fatal(u)
	}
	if got, _ := s.Accounts(ctx); len(got) != 2 || got[0].Login != "njreid" {
		t.Fatalf("users first: %+v", got)
	}
	if a, ok := s.Account(ctx, "PLANK-pattern"); !ok || a.Kind != "org" {
		t.Fatal("login lookup is case-insensitive")
	}
	if _, ok := s.Lookup("njreid"); ok {
		t.Fatal("no App yet, so nothing to look up")
	}
}

func TestExchangeRecordsTheAppOnItsAccount(t *testing.T) {
	var gotCode string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCode = strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/conversions"), "/app-manifests/")
		fmt.Fprintf(w, `{"id":42,"slug":"wrangl-plank-pattern","html_url":"https://github.com/apps/wrangl-plank-pattern","owner":{"login":"Plank-Pattern"},"pem":%q}`, pemKey(t))
	}))
	defer gh.Close()
	s := newSvc(t, gh.URL)
	ctx := context.Background()
	s.AddAccount(ctx, "plank-pattern", "org")
	changed := 0
	s.Changed = func() { changed++ }

	if _, err := s.Exchange(ctx, "abc", "bogus"); err == nil {
		t.Fatal("unknown state accepted")
	}
	st := s.NewState("plank-pattern", "/project/pnp")
	res, err := s.Exchange(ctx, "abc", st)
	if err != nil || res.App.ID != 42 || res.Login != "plank-pattern" || res.Back != "/project/pnp" || gotCode != "abc" {
		t.Fatalf("exchange: %+v %v", res, err)
	}
	if _, err := s.Exchange(ctx, "abc", st); err == nil {
		t.Fatal("state reused")
	}
	fi, err := os.Stat(filepath.Join(s.DataDir, "secrets/gh-plank-pattern.pem"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	if ref, ok := s.Lookup("plank-pattern"); !ok || ref.AppID != 42 || !strings.HasSuffix(ref.KeyPath, "gh-plank-pattern.pem") {
		t.Fatalf("lookup: %+v %v", ref, ok)
	}
	a, _ := s.Account(ctx, "plank-pattern")
	if a.App == nil {
		t.Fatal("app not recorded")
	}
	if !strings.HasSuffix(a.App.InstallURL(), "/wrangl-plank-pattern/installations/new") || a.InstallationsURL() != "https://github.com/organizations/plank-pattern/settings/installations" {
		t.Fatalf("%s %s", a.App.InstallURL(), a.InstallationsURL())
	}
	if changed == 0 {
		t.Fatal("Changed not called")
	}
}

func TestManifestAndNames(t *testing.T) {
	s := newSvc(t, "")
	m, err := s.Manifest("n")
	if err != nil || !strings.Contains(string(m), "https://ctrl.example.com/setup/github/callback") {
		t.Fatalf("manifest: %s %v", m, err)
	}
	if n := AppName("Plank-Pattern"); n != "wrangl-plank-pattern" {
		t.Fatal(n)
	}
	if n := AppName(strings.Repeat("x", 39)); len(n) > 34 {
		t.Fatal(n)
	}
}

func TestMigrateLegacyAdoptsTheOldApp(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app" {
			http.Error(w, "no", 404)
			return
		}
		w.Write([]byte(`{"slug":"wrangl-author-njreid","owner":{"login":"njreid","type":"User"}}`))
	}))
	defer gh.Close()
	s := newSvc(t, gh.URL)
	ctx := context.Background()
	if err := s.MigrateLegacy(ctx, 0, ""); err != nil {
		t.Fatalf("nothing to migrate must be fine: %v", err)
	}
	os.MkdirAll(filepath.Join(s.DataDir, "secrets"), 0o750)
	os.WriteFile(filepath.Join(s.DataDir, KeyRel), []byte(pemKey(t)), 0o600)
	os.WriteFile(filepath.Join(s.DataDir, metaFile), []byte(`{"id":5587772,"slug":"wrangl-author-njreid","html_url":"https://github.com/apps/wrangl-author-njreid"}`), 0o600)
	if err := s.MigrateLegacy(ctx, 0, ""); err != nil {
		t.Fatal(err)
	}
	a, ok := s.Account(ctx, "njreid")
	if !ok || a.Kind != "user" || a.App == nil || a.App.ID != 5587772 {
		t.Fatalf("migrated: %+v %v", a, ok)
	}
	if ref, ok := s.Lookup("njreid"); !ok || ref.KeyPath != filepath.Join(s.DataDir, KeyRel) {
		t.Fatalf("the legacy key stays where it is: %+v", ref)
	}
	if err := s.MigrateLegacy(ctx, 0, ""); err != nil {
		t.Fatal("running it again must be a no-op:", err)
	}
}

func TestSafeBack(t *testing.T) {
	for in, want := range map[string]string{"/project/pnp": "/project/pnp", "//evil.example": "", "https://evil.example": "", "": "", "/a\\b": "", "/ok?x=1": "/ok?x=1"} {
		if got := SafeBack(in); got != want {
			t.Errorf("SafeBack(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReviewerAppFlow(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":77,"slug":"wrangl-reviewer-njreid","html_url":"https://github.com/apps/wrangl-reviewer-njreid","owner":{"login":"njreid"},"webhook_secret":"s3cret","pem":%q}`, pemKey(t))
	}))
	defer gh.Close()
	s := newSvc(t, gh.URL)
	ctx := context.Background()
	s.AddAccount(ctx, "njreid", "user")
	if _, ok := s.LookupReviewer("njreid"); ok {
		t.Fatal("reviewer before creation")
	}
	m, _ := s.ManifestFor(ReviewerName("njreid"), "reviewer")
	for _, want := range []string{`"active":true`, `"checks":"read"`, "pull_request_review", "/gh/webhook"} {
		if !strings.Contains(string(m), want) {
			t.Errorf("manifest lacks %s: %s", want, m)
		}
	}
	res, err := s.Exchange(ctx, "c", s.NewRoleState("njreid", "/inbox", "reviewer"))
	if err != nil || res.Role != "reviewer" || res.Back != "/inbox" {
		t.Fatalf("%+v %v", res, err)
	}
	a, _ := s.Account(ctx, "njreid")
	if a.Reviewer == nil || a.Reviewer.ID != 77 || a.App != nil {
		t.Fatalf("reviewer must be recorded apart from the author App: %+v", a)
	}
	if ref, ok := s.LookupReviewer("njreid"); !ok || ref.AppID != 77 {
		t.Fatalf("%+v %v", ref, ok)
	}
	if _, ok := s.Lookup("njreid"); ok {
		t.Fatal("the reviewer App must not serve as the author App")
	}
	if sec := s.WebhookSecrets(); len(sec) != 1 || string(sec[0]) != "s3cret" {
		t.Fatalf("%q", sec)
	}
}

// The one App per account does everything but approve: it reads checks, merges and receives
// webhooks, so the reviewer App can stay optional.
func TestSingleAppManifestIsComplete(t *testing.T) {
	s := newSvc(t, "")
	b, err := s.Manifest("wrangl-acme")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Hook struct {
			URL    string `json:"url"`
			Active bool   `json:"active"`
		} `json:"hook_attributes"`
		Perms  map[string]string `json:"default_permissions"`
		Events []string          `json:"default_events"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if !m.Hook.Active || !strings.HasSuffix(m.Hook.URL, "/gh/webhook") {
		t.Fatalf("webhook: %+v", m.Hook)
	}
	for perm, want := range map[string]string{"contents": "write", "pull_requests": "write", "checks": "read", "issues": "write", "metadata": "read"} {
		if m.Perms[perm] != want {
			t.Errorf("permission %s = %q, want %q", perm, m.Perms[perm], want)
		}
	}
	if !strings.Contains(strings.Join(m.Events, ","), "pull_request") || !strings.Contains(strings.Join(m.Events, ","), "check_run") {
		t.Fatalf("events: %v", m.Events)
	}
}

// Nobody says whether an account is personal or an organisation: GitHub does.
func TestAddAccountDetectsKind(t *testing.T) {
	s := newSvc(t, "")
	ctx := context.Background()
	s.KindOf = func(_ context.Context, login string) (string, error) {
		if login == "acme" {
			return "org", nil
		}
		if login == "ghost" {
			return "", fmt.Errorf("GitHub has no user or organisation called %q", login)
		}
		return "user", nil
	}
	if a, err := s.AddAccount(ctx, "acme", ""); err != nil || a.Kind != "org" {
		t.Fatalf("org: %+v %v", a, err)
	}
	if a, err := s.AddAccount(ctx, "@someone", ""); err != nil || a.Kind != "user" {
		t.Fatalf("user: %+v %v", a, err)
	}
	if _, err := s.AddAccount(ctx, "ghost", ""); err == nil || !strings.Contains(err.Error(), "no user or organisation") {
		t.Fatalf("an unknown account must be refused with a reason: %v", err)
	}
	if a, err := s.AddAccount(ctx, "forced", "org"); err != nil || a.Kind != "org" { // an explicit kind skips the lookup
		t.Fatalf("explicit kind: %+v %v", a, err)
	}
}

func TestFreshAppNameStaysValid(t *testing.T) {
	for _, login := range []string{"njreid", "AgenticWrestling", strings.Repeat("x", 39)} {
		n := FreshAppName(login)
		if len(n) > 34 || !strings.HasPrefix(n, "wrangl-") || strings.HasSuffix(n, "-") || n == AppName(login) {
			t.Errorf("%s -> %q", login, n)
		}
	}
}

// The reviewer App only reads issues; the author App is the one that writes them.
func TestReviewerManifestKeepsIssuesRead(t *testing.T) {
	s := newSvc(t, "")
	b, err := s.ManifestFor("wrangl-acme-reviewer", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Perms map[string]string `json:"default_permissions"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Perms["issues"] != "read" {
		t.Fatalf("reviewer issues permission = %q", m.Perms["issues"])
	}
}

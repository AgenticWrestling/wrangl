package gh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// patGitHub is a fake that behaves like GitHub does for a personal access token: the
// App-only endpoints REFUSE it, and everything else accepts it as `Bearer <pat>` or
// `token <pat>`.
//
// The 401 body is GitHub's real one. That matters: the message blames the token's FORMAT
// ("could not be decoded"), not the credential type, so a reader meeting it in a log looks
// for a corrupt key rather than for a code path that should never have been taken. This fake
// exists to make that failure happen in a test instead.
func patGitHub(t *testing.T, pat string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	authed := func(r *http.Request) bool {
		h := r.Header.Get("Authorization")
		return h == "Bearer "+pat || h == "token "+pat
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		// The App-only surface. GitHub accepts an App JWT here and nothing else.
		if strings.HasSuffix(r.URL.Path, "/installation") || strings.HasPrefix(r.URL.Path, "/app") {
			http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
			return
		}
		if !authed(r) {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/app":
			fmt.Fprint(w, `{"default_branch":"trunk","clone_url":"https://github.com/acme/app.git","private":true}`)
		case "/repos/acme/app/pulls":
			fmt.Fprint(w, `[{"number":7,"title":"t","head":{"ref":"wrangl/x-1","sha":"abc"},"base":{"ref":"trunk"},"draft":false,"user":{"login":"nreidtwil"}}]`)
		case "/user":
			fmt.Fprint(w, `{"login":"nreidtwil","type":"User"}`)
		case "/orgs/acme/repos":
			fmt.Fprint(w, `[{"name":"app","default_branch":"trunk","clone_url":"u","private":true,"owner":{"login":"acme"}}]`)
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// TestPATModeTouchesNoAppOnlyEndpoint is the guard the tokenFor defect got past.
//
// It is deliberately NOT a check that each method returns the right value: it asserts over
// the SET OF ENDPOINTS the client reached, because that is the property that generalises.
// A new method that resolves an installation, or mints from /app/installations, fails here
// without anyone remembering that PAT mode exists -- which is what went wrong the first time,
// when mintToken was guarded and tokenFor, its caller, was not.
func TestPATModeTouchesNoAppOnlyEndpoint(t *testing.T) {
	const pat = "ghp_testtoken"
	srv, paths := patGitHub(t, pat)
	c := &Client{PAT: pat, BaseURL: srv.URL}
	ctx := context.Background()

	r, err := c.Repo(ctx, "acme", "app")
	if err != nil {
		t.Fatalf("Repo: %v", err)
	}
	if r.DefaultBranch != "trunk" || !r.Private {
		t.Errorf("Repo read wrong: %+v", r)
	}
	// InstallationID must stay zero: there is no installation, and a fabricated id would be
	// stored in the database and printed in logs as though it meant something.
	if r.InstallationID != 0 {
		t.Errorf("InstallationID = %d, want 0 in PAT mode", r.InstallationID)
	}

	tok, exp, err := c.RepoToken(ctx, r)
	if err != nil {
		t.Fatalf("RepoToken: %v", err)
	}
	if tok != pat {
		t.Errorf("RepoToken returned %q, want the PAT itself", tok)
	}
	if !exp.Equal(patExpiry) {
		t.Errorf("RepoToken expiry = %v, want patExpiry", exp)
	}

	// OpenPRs goes through tokenFor, which is the function that was missed. Before the fix
	// this call reached /repos/acme/app/installation and failed with the 401 above.
	prs, err := c.OpenPRs(ctx, r)
	if err != nil {
		t.Fatalf("OpenPRs (the tokenFor path): %v", err)
	}
	if len(prs) != 1 {
		t.Errorf("OpenPRs returned %d PRs, want 1", len(prs))
	}

	if login, kind, err := c.AppOwner(ctx); err != nil || login != "nreidtwil" || kind != "user" {
		t.Errorf("AppOwner = %q/%q, %v; want nreidtwil/user", login, kind, err)
	}
	if repos, err := c.ListRepos(ctx, "acme"); err != nil || len(repos) != 1 {
		t.Errorf("ListRepos = %d repos, %v; want 1", len(repos), err)
	}

	// The assertion this test exists for.
	got := paths()
	if len(got) == 0 {
		t.Fatal("no requests recorded: the fake was never reached, so every row above is vacuous")
	}
	for _, p := range got {
		if strings.HasSuffix(p, "/installation") || strings.HasPrefix(p, "/app") {
			t.Errorf("PAT mode reached the App-only endpoint %q; it accepts a JWT and nothing else", p)
		}
	}
}

// TestPATIsAFallbackAndNotAnOverride pins the polarity chosen in Multi.client: an owner that
// HAS an App keeps using it, and its per-repository, hour-long, least-privilege tokens. The
// opposite reading -- a configured PAT winning everywhere -- would silently downgrade every
// account on the controller to one broad long-lived credential.
func TestPATIsAFallbackAndNotAnOverride(t *testing.T) {
	// No HTTP here on purpose: the property is which CLIENT Multi hands back, which is decided
	// before any request is made, so a fake server would only add a way for this to fail for an
	// unrelated reason.
	keyPath := writeKey(t)
	m := &Multi{
		PAT: "ghp_testtoken",
		Lookup: func(owner string) (AppRef, bool) {
			if owner == "acme" {
				return AppRef{AppID: 42, KeyPath: keyPath}, true
			}
			return AppRef{}, false
		},
	}

	// acme has an App: the client must be the App one, not the PAT one.
	withApp, err := m.client("acme")
	if err != nil {
		t.Fatalf("client(acme): %v", err)
	}
	if withApp.usesPAT() {
		t.Error("acme has an App but got a PAT client: the PAT is overriding, not falling back")
	}
	if withApp.AppID != 42 {
		t.Errorf("acme client AppID = %d, want 42", withApp.AppID)
	}

	// An owner with no App falls back to the PAT rather than erroring.
	noApp, err := m.client("other")
	if err != nil {
		t.Fatalf("client(other) should fall back to the PAT, got %v", err)
	}
	if !noApp.usesPAT() {
		t.Error("owner with no App did not get a PAT client")
	}
}

// TestNoPATMeansTheOldRefusal checks the other direction: with no PAT configured, an owner
// with no App still gets *NoAppError. Without this row the fallback could swallow the error
// that tells an owner to create an App, and the two tests above would both still pass.
func TestNoPATMeansTheOldRefusal(t *testing.T) {
	m := &Multi{Lookup: func(string) (AppRef, bool) { return AppRef{}, false }}
	_, err := m.client("other")
	if err == nil {
		t.Fatal("want *NoAppError with no PAT configured, got nil")
	}
	var noApp *NoAppError
	if !errors.As(err, &noApp) {
		t.Errorf("want *NoAppError, got %T: %v", err, err)
	}
}

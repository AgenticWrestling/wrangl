package gh

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKey(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "k.pem")
	os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600)
	return p
}

func TestMultiRoutesByOwnerAndReportsAMissingApp(t *testing.T) {
	var asApp []string // which App id signed each request (iss claim is checked loosely via the path)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asApp = append(asApp, r.URL.Path)
		switch {
		case strings.ToLower(r.URL.Path) == "/users/acme/installation":
			http.NotFound(w, r) // acme is an organisation
		case strings.ToLower(r.URL.Path) == "/orgs/acme/installation":
			w.Write([]byte(`{"id":7}`))
		case r.URL.Path == "/app/installations/7/access_tokens":
			w.Write([]byte(`{"token":"t","expires_at":"2030-01-01T00:00:00Z"}`))
		case r.URL.Path == "/installation/repositories":
			w.Write([]byte(`{"repositories":[{"name":"site","default_branch":"main","clone_url":"https://github.com/acme/site.git","private":true,"owner":{"login":"acme"}}]}`))
		case r.URL.Path == "/users/nobody/installation", r.URL.Path == "/orgs/nobody/installation":
			http.NotFound(w, r)
		default:
			http.Error(w, "unexpected "+r.URL.Path, 500)
		}
	}))
	defer srv.Close()
	key := writeKey(t)
	m := &Multi{BaseURL: srv.URL, Lookup: func(owner string) (AppRef, bool) {
		switch strings.ToLower(owner) {
		case "acme":
			return AppRef{AppID: 7, KeyPath: key}, true
		case "nobody":
			return AppRef{AppID: 9, KeyPath: key}, true
		}
		return AppRef{}, false
	}}
	ctx := context.Background()

	// No App for this owner: a typed error the web app turns into a "set it up" link.
	_, err := m.Repo(ctx, "stranger", "x")
	var na *NoAppError
	if !errors.As(err, &na) || na.Owner != "stranger" {
		t.Fatalf("want NoAppError, got %v", err)
	}
	if _, _, err := m.RepoToken(ctx, Repo{Owner: "stranger", Name: "x"}); !errors.As(err, &na) {
		t.Fatalf("tokens too: %v", err)
	}

	// An org's App lists the repositories it can see (the user lookup 404s, the org one answers).
	repos, err := m.ListRepos(ctx, "ACME")
	if err != nil || len(repos) != 1 || repos[0].Owner != "acme" || repos[0].Name != "site" || !repos[0].Private || repos[0].InstallationID != 7 {
		t.Fatalf("repos: %+v %v", repos, err)
	}
	n := len(asApp)
	if _, err := m.ListRepos(ctx, "acme"); err != nil || len(asApp) != n {
		t.Fatal("the second listing is cached")
	}
	m.Forget()
	if _, err := m.ListRepos(ctx, "acme"); err != nil || len(asApp) == n {
		t.Fatal("Forget drops the cache")
	}

	// An App that is not installed on the account.
	_, err = m.ListRepos(ctx, "nobody")
	var ni *NotInstalledError
	if !errors.As(err, &ni) || !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("want NotInstalledError, got %v", err)
	}
}

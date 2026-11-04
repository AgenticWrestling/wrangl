package gh

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGitHub is an httptest server speaking just enough of the REST API, verifying the
// App JWT signature with the App's public key.
func fakeGitHub(t *testing.T, key *rsa.PrivateKey) *httptest.Server {
	verifyJWT := func(r *http.Request) bool {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(tok, ".")
		if len(parts) != 3 {
			return false
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
			return false
		}
		claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var c struct {
			Iss string `json:"iss"`
			Exp int64  `json:"exp"`
		}
		json.Unmarshal(claims, &c)
		return c.Iss == "42" && c.Exp > time.Now().Unix()
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/app/installation":
			if !verifyJWT(r) {
				http.Error(w, "bad jwt", 401)
				return
			}
			fmt.Fprint(w, `{"id": 777}`)
		case r.URL.Path == "/repos/acme/missing/installation":
			http.Error(w, `{"message":"Not Found"}`, 404)
		case r.URL.Path == "/app/installations/777/access_tokens" && r.Method == http.MethodPost:
			if !verifyJWT(r) {
				http.Error(w, "bad jwt", 401)
				return
			}
			var body struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Repositories) != 1 || body.Repositories[0] != "app" || body.Permissions["contents"] != "write" || body.Permissions["pull_requests"] != "write" {
				http.Error(w, "token request not scoped to one repo", 422)
				return
			}
			fmt.Fprint(w, `{"token":"ghs_scoped","expires_at":"2030-01-01T00:00:00Z"}`)
		case r.URL.Path == "/repos/acme/app" && r.Header.Get("Authorization") == "token ghs_scoped":
			fmt.Fprint(w, `{"default_branch":"trunk","clone_url":"https://github.com/acme/app.git","private":true}`)
		case r.URL.Path == "/repos/acme/app/issues" && r.Header.Get("Authorization") == "token ghs_scoped":
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, `[{"number":3,"title":"third","state":"open","html_url":"u3"}]`)
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/app/issues?state=open&page=2>; rel="next"`, srv.URL))
			fmt.Fprint(w, `[{"number":1,"title":"first","body":"b","state":"open","html_url":"u1"},
			                {"number":2,"title":"a PR","state":"open","html_url":"u2","pull_request":{"url":"x"}}]`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), 500)
		}
	}))
	return srv
}

func TestClientAgainstFakeGitHub(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := fakeGitHub(t, key)
	defer srv.Close()
	c := &Client{AppID: 42, Key: key, BaseURL: srv.URL}
	ctx := context.Background()

	r, err := c.Repo(ctx, "acme", "app")
	if err != nil {
		t.Fatal(err)
	}
	if r.InstallationID != 777 || r.DefaultBranch != "trunk" || !r.Private || r.CloneURL == "" {
		t.Fatalf("repo %+v", r)
	}
	tok, exp, err := c.RepoToken(ctx, r)
	if err != nil || tok != "ghs_scoped" || exp.Year() != 2030 {
		t.Fatalf("token %q %v %v", tok, exp, err)
	}
	issues, err := c.Issues(ctx, r, "open")
	if err != nil || len(issues) != 2 || issues[0].Number != 1 || issues[1].Number != 3 {
		t.Fatalf("issues (pagination, PRs skipped): %+v %v", issues, err)
	}
	if _, err := c.Repo(ctx, "acme", "missing"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("not installed: %v", err)
	}
	// A different key's JWT is rejected by the server.
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	if _, err := (&Client{AppID: 42, Key: other, BaseURL: srv.URL}).Repo(ctx, "acme", "app"); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestLoadKeyFormats(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	dir := t.TempDir()
	p1 := filepath.Join(dir, "pkcs1.pem")
	os.WriteFile(p1, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	p8 := filepath.Join(dir, "pkcs8.pem")
	os.WriteFile(p8, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
	for _, p := range []string{p1, p8} {
		k, err := LoadKey(p)
		if err != nil || k.N.Cmp(key.N) != 0 {
			t.Fatalf("%s: %v", p, err)
		}
	}
}

func TestClientIssueReads(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/app/installation":
			fmt.Fprint(w, `{"id": 777}`)
		case r.URL.Path == "/app/installations/777/access_tokens":
			fmt.Fprint(w, `{"token":"ghs_scoped","expires_at":"2030-01-01T00:00:00Z"}`)
		case r.URL.Path == "/repos/acme/app/issues/1":
			fmt.Fprint(w, `{"number":1,"title":"t","body":"b","state":"open","html_url":"u1","user":{"login":"ann"},"labels":[{"name":"bug"}],"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z","comments":3}`)
		case r.URL.Path == "/repos/acme/app/issues/2":
			fmt.Fprint(w, `{"number":2,"title":"pr","state":"open","pull_request":{"url":"x"}}`)
		case r.URL.Path == "/repos/acme/app/issues/404":
			http.Error(w, `{"message":"Not Found"}`, 404)
		case r.URL.Path == "/repos/acme/app/issues/1/comments":
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("Link", fmt.Sprintf(`<%s/x?page=2>; rel="next"`, srv.URL))
			}
			fmt.Fprint(w, `[{"id":9,"body":"hello","created_at":"2026-01-01T00:00:00Z","user":{"login":"bob"}}]`)
		case r.URL.Path == "/search/issues":
			q := r.URL.Query().Get("q")
			if !strings.Contains(q, "repo:acme/app") || !strings.Contains(q, "is:issue") || !strings.Contains(q, "is:open") {
				http.Error(w, "bad q "+q, 422)
				return
			}
			fmt.Fprint(w, `{"items":[{"number":1,"title":"t","state":"open","html_url":"u1"},{"number":2,"title":"pr","state":"open","pull_request":{"url":"x"}}]}`)
		default:
			http.Error(w, "unexpected "+r.URL.String(), 500)
		}
	}))
	defer srv.Close()
	c := &Client{AppID: 42, Key: key, BaseURL: srv.URL}
	ctx := context.Background()
	r := Repo{Owner: "acme", Name: "app", InstallationID: 777}
	i, err := c.GetIssue(ctx, r, 1)
	if err != nil || i.Author != "ann" || len(i.Labels) != 1 || i.UpdatedAt != "2026-01-02T00:00:00Z" || i.Comments != 3 {
		t.Fatalf("GetIssue: %+v %v", i, err)
	}
	if _, err := c.GetIssue(ctx, r, 2); !errors.Is(err, ErrIsPullRequest) {
		t.Fatalf("PR: %v", err)
	}
	if _, err := c.GetIssue(ctx, r, 404); !errors.Is(err, ErrIssueNotFound) {
		t.Fatalf("404: %v", err)
	}
	cs, more, err := c.IssueComments(ctx, r, 1, 1)
	if err != nil || len(cs) != 1 || cs[0].Author != "bob" || !more {
		t.Fatalf("comments: %+v %v %v", cs, more, err)
	}
	if _, more, _ = c.IssueComments(ctx, r, 1, 2); more {
		t.Fatal("page 2 has no next")
	}
	found, err := c.SearchIssues(ctx, r, "crash", "open", 10)
	if err != nil || len(found) != 1 || found[0].Number != 1 {
		t.Fatalf("search (PRs must be dropped): %+v %v", found, err)
	}
}

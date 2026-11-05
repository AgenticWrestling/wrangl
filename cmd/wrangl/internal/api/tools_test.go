package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/fleet"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

const toolKDL = `tool "deploy_status" {
  description "Show the deployment status"
  effect "read"
  run-kinds "work"
  input { param "env" type="string" { enum "staging" "prod" } }
  exec { argv "kubectl" "get" "deploy" "-n" "{{input.env}}"; secret "KUBECONFIG" name="kube.prod" as="file" }
}`

func toolServer(t *testing.T) *http.ServeMux {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.W.Exec(`INSERT INTO project (slug, name) VALUES ('alpha', 'Alpha')`); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerTools(mux, Deps{Fleet: &fleet.Service{DB: d}})
	return mux
}

func send(mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func kdlBody(k string) string {
	b, _ := json.Marshal(proto.LibraryToolRequest{KDL: k})
	return string(b)
}

func TestToolEndpoints(t *testing.T) {
	mux := toolServer(t)
	rec := send(mux, "POST", "/api/v1/tools", kdlBody(toolKDL))
	if rec.Code != 201 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	var v proto.LibraryToolView
	json.Unmarshal(rec.Body.Bytes(), &v)
	if v.Name != "deploy_status" || v.Kind != "exec" || v.Version != 1 || len(v.RunKinds) != 1 || v.KDL == "" {
		t.Fatalf("view: %+v", v)
	}
	if rec = send(mux, "POST", "/api/v1/tools", kdlBody(toolKDL)); rec.Code != 409 {
		t.Fatalf("duplicate: %d", rec.Code)
	}
	if rec = send(mux, "POST", "/api/v1/tools", kdlBody(`tool "x" { exec { argv "sh" "-c" "id" } }`)); rec.Code != 400 {
		t.Fatalf("invalid: %d %s", rec.Code, rec.Body)
	}

	// list and show; the list carries no source, the declaration carries no executor
	rec = send(mux, "GET", "/api/v1/tools", "")
	var list []proto.LibraryToolView
	json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != 200 || len(list) != 1 || list[0].KDL != "" || list[0].Projects == nil {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	rec = send(mux, "GET", "/api/v1/tools/deploy_status", "")
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	json.Unmarshal(rec.Body.Bytes(), &v)
	if strings.Contains(string(v.Declaration), "kubectl") || !strings.Contains(string(v.Declaration), "staging") {
		t.Fatalf("declaration: %s", v.Declaration)
	}
	if rec = send(mux, "GET", "/api/v1/tools/ghost", ""); rec.Code != 404 {
		t.Fatalf("show unknown: %d", rec.Code)
	}

	// edit
	if rec = send(mux, "PUT", "/api/v1/tools/ghost", "not json"); rec.Code != 404 {
		t.Fatalf("edit unknown: %d", rec.Code)
	}
	rec = send(mux, "PUT", "/api/v1/tools/deploy_status", kdlBody(strings.Replace(toolKDL, "Show", "Display", 1)))
	json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != 200 || v.Version != 2 || !strings.HasPrefix(v.Description, "Display") {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body)
	}
	if rec = send(mux, "PUT", "/api/v1/tools/deploy_status", kdlBody(strings.Replace(toolKDL, "deploy_status", "renamed", 1))); rec.Code != 400 {
		t.Fatalf("rename: %d", rec.Code)
	}

	// enablement, test, delete
	if rec = send(mux, "POST", "/api/v1/projects/alpha/tools/deploy_status/test", `{}`); rec.Code != 409 {
		t.Fatalf("test before enable: %d %s", rec.Code, rec.Body)
	}
	if rec = send(mux, "PUT", "/api/v1/projects/alpha/tools/deploy_status", ""); rec.Code != 200 {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body)
	}
	if rec = send(mux, "PUT", "/api/v1/projects/nope/tools/deploy_status", ""); rec.Code != 404 {
		t.Fatalf("enable unknown project: %d", rec.Code)
	}
	rec = send(mux, "GET", "/api/v1/projects/alpha/tools", "")
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Projects[0] != "alpha" {
		t.Fatalf("project tools: %s", rec.Body)
	}
	if rec = send(mux, "POST", "/api/v1/projects/alpha/tools/deploy_status/test", `{"input":{"env":"prod"}}`); rec.Code != 501 || !strings.Contains(rec.Body.String(), "not implemented") {
		t.Fatalf("test: %d %s", rec.Code, rec.Body)
	}
	if rec = send(mux, "DELETE", "/api/v1/tools/deploy_status", ""); rec.Code != 409 {
		t.Fatalf("delete enabled: %d", rec.Code)
	}
	if rec = send(mux, "DELETE", "/api/v1/projects/alpha/tools/deploy_status", ""); rec.Code != 204 {
		t.Fatalf("disable: %d", rec.Code)
	}
	if rec = send(mux, "DELETE", "/api/v1/tools/deploy_status", ""); rec.Code != 204 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec = send(mux, "GET", "/api/v1/tools/deploy_status", ""); rec.Code != 404 {
		t.Fatalf("after delete: %d", rec.Code)
	}
}

// memStore is an in-memory secret store for tests.
type memStore struct{ m map[string]string }

func (s *memStore) store() *SecretStore {
	return &SecretStore{
		Set: func(name string, r io.Reader) error {
			b, _ := io.ReadAll(r)
			s.m[name] = strings.TrimSpace(string(b))
			return nil
		},
		List: func() ([]string, error) { return nil, nil },
		Has:  func(n string) bool { _, ok := s.m[n]; return ok },
		Get: func(n string) ([]byte, error) {
			if v, ok := s.m[n]; ok {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		},
	}
}

func TestToolSecretsNeverLeak(t *testing.T) {
	const value = "s3cr3t-VALUE-9f2c1d"
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	d.W.Exec(`INSERT INTO project (slug, name) VALUES ('alpha', 'Alpha'), ('beta', 'Beta')`)
	ms := &memStore{m: map[string]string{}}
	mux := http.NewServeMux()
	registerTools(mux, Deps{Fleet: &fleet.Service{DB: d}, Secrets: ms.store()})

	var responses []string
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := send(mux, method, path, body)
		responses = append(responses, rec.Body.String())
		for k, v := range rec.Header() {
			responses = append(responses, k+strings.Join(v, ","))
		}
		return rec
	}
	if rec := do("POST", "/api/v1/tools", kdlBody(toolKDL)); rec.Code != 201 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}

	// enabling reports the missing secret
	rec := do("PUT", "/api/v1/projects/alpha/tools/deploy_status", "")
	var v proto.LibraryToolView
	json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != 200 || len(v.Secrets) != 1 || v.Secrets[0].Name != "kube.prod" || v.Secrets[0].Set || v.Secrets[0].Env != "KUBECONFIG" || v.Secrets[0].As != "file" {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body)
	}

	// set it: namespaced per project, only for declared names
	if rec := do("PUT", "/api/v1/projects/alpha/tool-secrets/kube.prod", value+"\n"); rec.Code != 204 {
		t.Fatalf("set: %d %s", rec.Code, rec.Body)
	}
	if got := ms.m["tool.alpha.kube.prod"]; got != value {
		t.Fatalf("stored under the wrong name: %v", ms.m)
	}
	if len(ms.m) != 1 {
		t.Fatalf("unexpected store writes: %v", ms.m)
	}
	for _, c := range []struct {
		path, body string
		code       int
	}{
		{"/api/v1/projects/alpha/tool-secrets/other", value, 400},              // undeclared name: the value must not echo in the error
		{"/api/v1/projects/alpha/tool-secrets/..%2Fx", value, 400},             // traversal attempt
		{"/api/v1/projects/nope/tool-secrets/kube.prod", value, 404},           // unknown project
		{"/api/v1/projects/alpha/tool-secrets/kube.prod", "   ", 400},          // empty
		{"/api/v1/projects/alpha/tool-secrets/claude_oauth_token", value, 400}, // global secrets are out of reach
	} {
		if rec := do("PUT", c.path, c.body); rec.Code != c.code {
			t.Errorf("PUT %s: %d %s, want %d", c.path, rec.Code, rec.Body, c.code)
		}
	}
	if len(ms.m) != 1 {
		t.Fatalf("rejected sets wrote: %v", ms.m)
	}

	// status: alpha set, beta missing
	var st []proto.LibraryToolSecret
	rec = do("GET", "/api/v1/projects/alpha/tools/deploy_status/secrets", "")
	json.Unmarshal(rec.Body.Bytes(), &st)
	if rec.Code != 200 || len(st) != 1 || !st[0].Set {
		t.Fatalf("alpha status: %s", rec.Body)
	}
	do("PUT", "/api/v1/projects/beta/tools/deploy_status", "")
	rec = do("GET", "/api/v1/projects/beta/tools/deploy_status/secrets", "")
	json.Unmarshal(rec.Body.Bytes(), &st)
	if len(st) != 1 || st[0].Set {
		t.Fatalf("beta must not see alpha's secret: %s", rec.Body)
	}
	rec = do("GET", "/api/v1/projects/alpha/tools", "")
	json.Unmarshal(rec.Body.Bytes(), &[]proto.LibraryToolView{})
	for _, p := range []string{"/api/v1/tools", "/api/v1/tools/deploy_status", "/api/v1/projects/beta/tools", "/api/v1/projects/alpha/tools/nope/secrets"} {
		do("GET", p, "")
	}

	for i, r := range responses {
		if strings.Contains(r, value) {
			t.Errorf("response %d contains the secret value: %s", i, r)
		}
	}
	if strings.Contains(logs.String(), value) {
		t.Errorf("log contains the secret value:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "tool secret set") {
		t.Errorf("expected an audit log line without the value, got %q", logs.String())
	}
}

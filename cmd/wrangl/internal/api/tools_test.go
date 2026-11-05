package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if rec = send(mux, "PUT", "/api/v1/projects/alpha/tools/deploy_status", ""); rec.Code != 204 {
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

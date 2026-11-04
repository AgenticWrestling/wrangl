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
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/projects"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func archiveServer(t *testing.T) (*http.ServeMux, *db.DB) {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, q := range []string{
		`INSERT INTO project (slug, name) VALUES ('alpha', 'Alpha'), ('beta', 'Beta')`,
		`INSERT INTO task (project_id, rank, title, sqid) VALUES (1, 'V', 'ta', 'AAAAAA')`,
	} {
		if _, err := d.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	registerWork(mux, Deps{Fleet: &fleet.Service{DB: d}, Projects: &projects.Service{DB: d}})
	return mux, d
}

func call(mux *http.ServeMux, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader("")))
	return rec
}

func slugs(t *testing.T, mux *http.ServeMux, q string) []string {
	t.Helper()
	rec := call(mux, "GET", "/api/v1/projects"+q)
	if rec.Code != 200 {
		t.Fatalf("list%s: %d %s", q, rec.Code, rec.Body)
	}
	var ps []proto.ProjectView
	json.Unmarshal(rec.Body.Bytes(), &ps)
	var out []string
	for _, p := range ps {
		out = append(out, p.Slug)
	}
	return out
}

func TestArchiveRestoreEndpoints(t *testing.T) {
	mux, _ := archiveServer(t)
	rec := call(mux, "POST", "/api/v1/projects/alpha/archive")
	if rec.Code != 200 {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body)
	}
	var res proto.ProjectArchiveResult
	json.Unmarshal(rec.Body.Bytes(), &res)
	if !res.Project.Archived || res.CancelledRuns == nil {
		t.Fatalf("result: %+v", res)
	}
	if got := slugs(t, mux, ""); len(got) != 1 || got[0] != "beta" {
		t.Fatalf("default list: %v", got)
	}
	if got := slugs(t, mux, "?archived=archived"); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("archived list: %v", got)
	}
	if got := slugs(t, mux, "?archived=all"); len(got) != 2 {
		t.Fatalf("all list: %v", got)
	}
	if rec := call(mux, "GET", "/api/v1/projects?archived=bogus"); rec.Code != 400 {
		t.Fatalf("bogus filter: %d", rec.Code)
	}
	// direct lookup still works and flags it
	rec = call(mux, "GET", "/api/v1/projects/alpha")
	var pv proto.ProjectView
	json.Unmarshal(rec.Body.Bytes(), &pv)
	if rec.Code != 200 || !pv.Archived {
		t.Fatalf("lookup: %d %s", rec.Code, rec.Body)
	}
	if rec := call(mux, "POST", "/api/v1/projects/alpha/archive"); rec.Code != 409 {
		t.Fatalf("double archive: %d", rec.Code)
	}
	if rec := call(mux, "POST", "/api/v1/projects/alpha/restore"); rec.Code != 200 {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	if rec := call(mux, "POST", "/api/v1/projects/alpha/restore"); rec.Code != 409 {
		t.Fatalf("restore active: %d", rec.Code)
	}
	if got := slugs(t, mux, ""); len(got) != 2 {
		t.Fatalf("after restore: %v", got)
	}
	if rec := call(mux, "POST", "/api/v1/projects/nope/archive"); rec.Code != 404 {
		t.Fatalf("unknown: %d", rec.Code)
	}
}

func TestArchiveRefusedListsActiveRuns(t *testing.T) {
	mux, d := archiveServer(t)
	d.W.Exec(`INSERT INTO run (task_id, state) VALUES (1, 'running')`)
	d.W.Exec(`INSERT INTO run (task_id, state) VALUES (1, 'queued')`)
	rec := call(mux, "POST", "/api/v1/projects/alpha/archive")
	var ref proto.ProjectArchiveRefusal
	json.Unmarshal(rec.Body.Bytes(), &ref)
	if rec.Code != 409 || len(ref.Runs) != 1 || ref.Runs[0].State != "running" || ref.Runs[0].Task != "AAAAAA" || ref.Error == "" {
		t.Fatalf("refusal: %d %s", rec.Code, rec.Body)
	}
	if got := slugs(t, mux, ""); len(got) != 2 {
		t.Fatalf("project hidden despite refusal: %v", got)
	}
}

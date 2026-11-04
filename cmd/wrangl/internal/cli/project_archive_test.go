package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func archiveAPI(t *testing.T, refuse bool, gotQuery *string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodGet:
			*gotQuery = r.URL.RawQuery
			json.NewEncoder(w).Encode([]proto.ProjectView{{Slug: "old", Name: "Old", Archived: true}})
		case r.URL.Path == "/api/v1/projects/p/archive":
			if refuse {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(proto.ProjectArchiveRefusal{Error: "project \"p\" has active runs",
					Runs: []proto.ProjectArchiveRunView{{ID: 412, State: "running", Task: "k3Fa9x", Title: "Add retry"}}})
				return
			}
			json.NewEncoder(w).Encode(proto.ProjectArchiveResult{Project: proto.ProjectView{Slug: "p", Archived: true}, CancelledRuns: []int64{5, 6}})
		case r.URL.Path == "/api/v1/projects/p/restore":
			json.NewEncoder(w).Encode(proto.ProjectView{Slug: "p"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestProjectArchiveRestoreList(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var q string
	url := archiveAPI(t, false, &q)
	out, err := run2(t, url, "project", "archive", "p")
	if err != nil || !strings.Contains(out, "archived p") || !strings.Contains(out, "2 queued") {
		t.Fatalf("archive: %v %q", err, out)
	}
	out, err = run2(t, url, "project", "restore", "p")
	if err != nil || !strings.Contains(out, "restored p") {
		t.Fatalf("restore: %v %q", err, out)
	}
	for args, want := range map[string]string{"": "archived=active", "--archived": "archived=archived", "--all": "archived=all"} {
		a := []string{"project", "list"}
		if args != "" {
			a = append(a, args)
		}
		out, err = run2(t, url, a...)
		if err != nil || q != want || !strings.Contains(out, "old") || !strings.Contains(out, "archived") {
			t.Fatalf("list %q: %v query=%q out=%q", args, err, q, out)
		}
	}
	if _, err = run2(t, url, "project", "list", "--archived", "--all"); err == nil {
		t.Fatal("--archived with --all must be refused")
	}
}

func TestProjectArchiveRefusalListsRuns(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var q string
	url := archiveAPI(t, true, &q)
	out, err := run2(t, url, "project", "archive", "p")
	if err == nil {
		t.Fatal("archive must fail while runs are active")
	}
	all := out + err.Error()
	if !strings.Contains(all, "run 412") || !strings.Contains(all, "k3Fa9x") {
		t.Fatalf("refusal must list the run: out=%q err=%v", out, err)
	}
}

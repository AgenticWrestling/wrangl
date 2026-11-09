package web

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/api"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/diffs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestChangesSection(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	gitIn(t, src, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one\ntwo\n"), 0o644)
	gitIn(t, src, "add", ".")
	gitIn(t, src, "commit", "-qm", "base")
	base := gitIn(t, src, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one\n<b>TWO</b>\n"), 0o644)
	os.WriteFile(filepath.Join(src, "bin"), []byte("\x00\x01"), 0o644)
	gitIn(t, src, "add", ".")
	gitIn(t, src, "commit", "-qm", "work")
	head := gitIn(t, src, "rev-parse", "HEAD")

	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	d.W.Exec(`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('t','','{}',1,1,1)`)
	d.W.Exec(`INSERT INTO project (slug, name, default_template_id) VALUES ('p','P',1)`)
	d.W.Exec(`INSERT INTO repo (project_id, owner, name, default_branch, clone_url) VALUES (1,'acme','app','main',?)`, "file://"+src)
	d.W.Exec(`INSERT INTO task (project_id, rank, title, sqid) VALUES (1,'V001','a task','abc123')`)
	d.W.Exec(`INSERT INTO run (task_id, state, repo_id, branch, base_sha, head_sha) VALUES (1,'succeeded',1,'wrangl/a',?,?)`, base, head)
	h, err := New(api.Deps{Diffs: &diffs.Service{DB: d, Dir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}

	// The card shows a lazy Changes section only when it has code to show.
	with, _ := h.render("icard", inboxItem{DOM: "pr-1", Key: "pr1", Kind: "pr_review", Diff: changesURL(0, 1)})
	if !strings.Contains(string(with), `data-on:toggle__once`) || !strings.Contains(string(with), `id="chg-pr1"`) || !strings.Contains(string(with), "data-ignore-morph") {
		t.Fatalf("no Changes section:\n%s", with)
	}
	without, _ := h.render("icard", inboxItem{DOM: "q2", Key: "q2", Kind: "task_review"})
	if strings.Contains(string(without), "Changes") {
		t.Fatalf("section shown without a branch:\n%s", without)
	}

	rec := httptest.NewRecorder()
	h.changes(rec, httptest.NewRequest("GET", "/ui/changes?run=1&k=q2", nil))
	body := rec.Body.String()
	for _, want := range []string{"selector #chg-q2", "mode inner", "a.txt", "+1", "−1", "d-add", "&lt;b&gt;TWO", "Binary file not shown", "work", "commit="} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<b>TWO") {
		t.Error("diff text not escaped")
	}

	rec = httptest.NewRecorder()
	h.changes(rec, httptest.NewRequest("GET", "/ui/changes?run=99&k=q2", nil))
	if !strings.Contains(rec.Body.String(), "Couldn't load") {
		t.Errorf("error not shown: %s", rec.Body.String())
	}
}

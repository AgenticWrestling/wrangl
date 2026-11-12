package web

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/api"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/prs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// Questions that block a PR are sub-items of its review card, not top-level Inbox items; a
// question whose PR is closed falls back to the top level.
func TestBlockingQuestionsNestUnderTheReviewCard(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	d.W.Exec(`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('t','','{}',1,1,1)`)
	d.W.Exec(`INSERT INTO project (slug, name, default_template_id) VALUES ('p','P',1)`)
	d.W.Exec(`INSERT INTO repo (project_id, owner, name, default_branch, clone_url) VALUES (1,'acme','app','main','x')`)
	d.W.Exec(`INSERT INTO task (project_id, rank, title, sqid) VALUES (1,'V001','a task','abc123')`)
	d.W.Exec(`INSERT INTO run (task_id, state) VALUES (1,'succeeded')`)
	d.W.Exec(`INSERT INTO pull_request (task_id, repo_id, run_id, gh_number, url, head_branch, mergeable) VALUES (1,1,1,7,'u','wrangl/a','conflicted')`)
	h, err := New(api.Deps{PRs: &prs.Service{DB: d, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := d.RQ.ListPRs(ctx, "open")
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %d", err, len(rows))
	}
	qs := []proto.QuestionView{
		{ID: 1, Kind: "agent", Prompt: "Keep which side?", PRID: 1, AllowFreeText: true, Options: []proto.QuestionOption{{Label: "Ours", Value: "ours"}}},
		{ID: 2, Kind: "agent", Prompt: "Unrelated question"},
		{ID: 3, Kind: "agent", Prompt: "PR is gone", PRID: 99},
	}
	items := h.inboxItems(ctx, qs, rows, nil)
	var top []string
	var review *inboxItem
	for i, it := range items {
		top = append(top, it.DOM)
		if it.Kind == "pr_review" {
			review = &items[i]
		}
	}
	if strings.Join(top, ",") != "q2,q3,pr-1" {
		t.Fatalf("top-level items %v", top)
	}
	if len(review.Subs) != 1 || review.Subs[0].DOM != "q1" || len(review.Subs[0].Actions) != 1 {
		t.Fatalf("sub-items %+v", review.Subs)
	}
	out, err := h.render("icard", *review)
	if err != nil {
		t.Fatal(err)
	}
	html := string(out)
	if !strings.Contains(html, `class="isubs"`) || strings.Index(html, `id="q1"`) < strings.Index(html, `class="isubs"`) {
		t.Fatalf("question card not nested in the review card:\n%s", html)
	}
}

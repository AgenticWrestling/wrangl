package issuestore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
)

func open(t *testing.T) (*db.DB, int64) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	var pid, rid int64
	if err := d.W.QueryRow(`INSERT INTO project (slug, name) VALUES ('web', 'web') RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if err := d.W.QueryRow(`INSERT INTO repo (project_id, owner, name, default_branch, clone_url) VALUES (?, 'acme', 'app', 'main', 'x') RETURNING id`, pid).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	return d, rid
}

var now = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func TestSaveOnlyNewerWins(t *testing.T) {
	ctx := context.Background()
	d, repo := open(t)
	save := func(title, updated string) bool {
		_, applied, err := Save(ctx, d, repo, gh.Issue{Number: 1, Title: title, State: "open", URL: "u", UpdatedAt: updated, Author: "ann", Labels: []string{"bug"}, Comments: 2}, now)
		if err != nil {
			t.Fatal(err)
		}
		return applied
	}
	if !save("v1", "2026-11-04T10:00:00Z") {
		t.Fatal("first save must apply")
	}
	if save("older", "2026-11-04T09:00:00Z") {
		t.Fatal("older update must not apply")
	}
	if save("echo", "2026-11-04T10:00:00+00:00") {
		t.Fatal("same updated_at (our own write echoing back) must not apply")
	}
	if !save("v2", "2026-11-04T11:00:00Z") {
		t.Fatal("newer must apply")
	}
	row, err := Get(ctx, d, repo, 1)
	if err != nil || row.Title != "v2" || row.Author != "ann" || len(row.Labels) != 1 || row.CommentCount != 2 || row.GhUpdatedAt != "2026-11-04T11:00:00Z" {
		t.Fatalf("row: %+v %v", row, err)
	}
	// A row with no metadata (synced before issue_meta) is always overwritten.
	d.W.Exec(`DELETE FROM issue_meta`)
	if !save("v3", "2026-11-04T01:00:00Z") {
		t.Fatal("unknown updated_at must be overwritten")
	}
}

func TestSearchFTS(t *testing.T) {
	ctx := context.Background()
	d, repo := open(t)
	var pid int64
	d.R.QueryRow(`SELECT project_id FROM repo WHERE id = ?`, repo).Scan(&pid)
	Save(ctx, d, repo, gh.Issue{Number: 1, Title: "Crash on startup", Body: "panics while loading configs", State: "open", URL: "u1"}, now)
	Save(ctx, d, repo, gh.Issue{Number: 2, Title: "Docs typo", Body: "fix readme", State: "closed", URL: "u2"}, now)
	hits, err := Search(ctx, d, pid, 0, FTSQuery("loading config"), "all", 10)
	if err != nil || len(hits) != 1 || hits[0].Number != 1 { // porter stemming: configs ~ config
		t.Fatalf("hits: %+v %v", hits, err)
	}
	if hits, _ := Search(ctx, d, pid, 0, FTSQuery("crash"), "closed", 10); len(hits) != 0 {
		t.Fatalf("state filter: %+v", hits)
	}
	// An update re-indexes; a delete removes.
	Save(ctx, d, repo, gh.Issue{Number: 2, Title: "Docs typo", Body: "fix tutorial", State: "closed", URL: "u2"}, now)
	if hits, _ := Search(ctx, d, pid, 0, FTSQuery("tutorial"), "all", 10); len(hits) != 1 {
		t.Fatalf("after update: %+v", hits)
	}
	if hits, _ := Search(ctx, d, pid, 0, FTSQuery("readme"), "all", 10); len(hits) != 0 {
		t.Fatalf("stale index: %+v", hits)
	}
	d.W.Exec(`DELETE FROM issue WHERE gh_number = 2`)
	if hits, _ := Search(ctx, d, pid, 0, FTSQuery("tutorial"), "all", 10); len(hits) != 0 {
		t.Fatalf("after delete: %+v", hits)
	}
	// Hostile input is plain text, never an FTS syntax error.
	for _, q := range []string{`"`, `crash AND`, `NEAR(a b)`, `title:crash`, `cra*`, `-crash`, `(`, `a" OR "b`} {
		m := FTSQuery(q)
		if m == "" {
			continue
		}
		if _, err := Search(ctx, d, pid, 0, m, "all", 10); err != nil {
			t.Errorf("query %q: %v", q, err)
		}
	}
	if FTSQuery(`"*()`) != "" {
		t.Error("punctuation only must be empty")
	}
}

func TestCommentsCache(t *testing.T) {
	ctx := context.Background()
	d, repo := open(t)
	id, _, _ := Save(ctx, d, repo, gh.Issue{Number: 1, Title: "t", State: "open", URL: "u", UpdatedAt: "2026-11-04T10:00:00Z", Comments: 2}, now)
	if err := ReplaceComments(ctx, d, id, []gh.IssueComment{{ID: 5, Author: "a", Body: "x"}, {ID: 9, Author: "b", Body: "y"}}, true); err != nil {
		t.Fatal(err)
	}
	cs, _ := CachedComments(ctx, d, id, 5, 10)
	if len(cs) != 1 || cs[0].GhID != 9 {
		t.Fatalf("after cursor: %+v", cs)
	}
	if _, err := Get(ctx, d, repo, 99); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing: %v", err)
	}
}

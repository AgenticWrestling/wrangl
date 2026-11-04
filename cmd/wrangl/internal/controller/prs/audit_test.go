package prs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
)

func TestExtractCitations(t *testing.T) {
	diff := `--- a/research/x.md
+++ b/research/x.md
@@
-old https://old.example/removed
+See [the paper](https://arxiv.org/abs/1234.5678), and https://example.com/a?b=1.
+Bare: https://example.org/page, again https://arxiv.org/abs/1234.5678
 context https://context.example/not-added
+++ not a url line https://ignored.example
`
	got := ExtractCitations(diff)
	want := []string{"https://arxiv.org/abs/1234.5678", "https://example.com/a?b=1", "https://example.org/page"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestAuditOKAndMarkdown(t *testing.T) {
	good := Audit{Done: true, Results: []CitationResult{{URL: "https://a", Status: CiteVerified}}}
	if !good.OK() || !strings.Contains(auditMarkdown(good, true), "All 1 cited URL(s) verified") {
		t.Fatal("verified audit should be ok")
	}
	bad := Audit{Done: true, Results: []CitationResult{{URL: "https://a", Status: CiteVerified}, {URL: "https://b", Status: CiteUnsupported, Note: "says the opposite"}}, Missing: []string{"https://c"}}
	md := auditMarkdown(bad, true)
	if bad.OK() || !strings.Contains(md, "https://b: **unsupported** — says the opposite") || !strings.Contains(md, "https://c: **not checked**") {
		t.Fatalf("ok=%v\n%s", bad.OK(), md)
	}
	if (Audit{}).OK() || !strings.Contains(auditMarkdown(Audit{}, false), "ended without reporting") {
		t.Fatal("an audit that never reported is not ok")
	}
}

// auditAPI is a GitHub whose PR #7 is open, green, touches only research/, and cites two URLs.
type auditAPI struct {
	gh.API
	merges int
}

func (a *auditAPI) GetPR(context.Context, gh.Repo, int64) (gh.PR, error) {
	return gh.PR{Number: 7, HeadSHA: "abc", State: "open"}, nil
}
func (a *auditAPI) Checks(context.Context, gh.Repo, string) (string, error) {
	return gh.ChecksSuccess, nil
}
func (a *auditAPI) PRFiles(context.Context, gh.Repo, int64) ([]gh.PRFile, error) {
	return []gh.PRFile{{Path: "research/report.md", Additions: 5}}, nil
}
func (a *auditAPI) PRDiff(context.Context, gh.Repo, int64) (string, error) {
	return "+++ b/research/report.md\n+Source: https://example.com/one and https://example.com/two\n", nil
}
func (a *auditAPI) Merge(context.Context, gh.Repo, int64, string) error { a.merges++; return nil }

// With the audit on, a reviewed low-risk research PR is not auto-merged until an audit run reports
// every citation verified; the audit is queued once, as a read-only review run carrying the URLs.
func TestResearchAuditGatesAutoMerge(t *testing.T) {
	s, pr := mergeFixture(t)
	ctx := context.Background()
	api := &auditAPI{}
	s.Author = api
	s.DB.W.Exec(`UPDATE task SET task_type = 'research'`)
	s.DB.W.Exec(`UPDATE project SET auto_merge = 1, citation_audit = 1`)
	s.DB.W.Exec(`UPDATE pull_request SET head_sha = 'abc', reviewed_sha = 'abc', risk = 'low'`)

	if err := s.Evaluate(ctx, pr.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	var prompt string
	s.DB.R.QueryRow(`SELECT COUNT(*), COALESCE(MAX(prompt_override), '') FROM run WHERE kind = 'review' AND pr_id = ?`, pr.ID).Scan(&n, &prompt)
	if n != 1 || api.merges != 0 || !strings.Contains(prompt, "https://example.com/one") || !strings.Contains(prompt, "https://example.com/two") || !strings.Contains(prompt, "submit_citation_audit") {
		t.Fatalf("want one audit run listing both URLs and no merge: runs=%d merges=%d\n%s", n, api.merges, prompt)
	}
	// still running: Evaluate does not queue a second one, nor merge
	if err := s.Evaluate(ctx, pr.ID); err != nil {
		t.Fatal(err)
	}
	s.DB.R.QueryRow(`SELECT COUNT(*) FROM run WHERE kind = 'review' AND pr_id = ?`, pr.ID).Scan(&n)
	if n != 1 || api.merges != 0 {
		t.Fatalf("runs=%d merges=%d", n, api.merges)
	}
	// the auditor reports every URL verified, the run ends, Evaluate merges
	var runID int64
	s.DB.R.QueryRow(`SELECT id FROM run WHERE kind = 'review' AND pr_id = ?`, pr.ID).Scan(&runID)
	args, _ := json.Marshal(map[string]any{"citations": []map[string]string{
		{"url": "https://example.com/one", "status": "verified"}, {"url": "https://example.com/two", "status": "verified"}}})
	if _, err := s.SubmitCitationAudit(ctx, runs.Scope{RunID: runID, Kind: "review"}, args); err != nil {
		t.Fatal(err)
	}
	s.DB.W.Exec(`UPDATE run SET state = 'succeeded' WHERE id = ?`, runID)
	if err := s.Evaluate(ctx, pr.ID); err != nil {
		t.Fatal(err)
	}
	if api.merges != 1 {
		t.Fatalf("a fully verified audit should allow the auto-merge, merges=%d", api.merges)
	}
}

// An audit that leaves a URL unreported (or marks one unsupported) is stored as not ok.
func TestSubmitCitationAuditFlagsMissingAndUnsupported(t *testing.T) {
	s, pr := mergeFixture(t)
	ctx := context.Background()
	s.Author = &auditAPI{}
	s.DB.W.Exec(`UPDATE pull_request SET head_sha = 'abc'`)
	s.DB.W.Exec(`INSERT INTO run (task_id, kind, state, pr_id) VALUES (1, 'review', 'running', ?)`, pr.ID)
	var runID int64
	s.DB.R.QueryRow(`SELECT MAX(id) FROM run`).Scan(&runID)
	args, _ := json.Marshal(map[string]any{"citations": []map[string]string{{"url": "https://example.com/one", "status": "unsupported", "note": "page says X"}}})
	if _, err := s.SubmitCitationAudit(ctx, runs.Scope{RunID: runID}, args); err != nil {
		t.Fatal(err)
	}
	pr, _ = s.DB.RQ.GetPR(ctx, pr.ID)
	a, ok := s.loadAudit(ctx, pr)
	if !ok || a.OK() || len(a.Missing) != 1 || a.Missing[0] != "https://example.com/two" || len(a.Unverified()) != 2 {
		t.Fatalf("%+v ok=%v", a, ok)
	}
	if _, err := s.SubmitCitationAudit(ctx, runs.Scope{RunID: runID}, []byte(`{"citations":[{"url":"u","status":"maybe"}]}`)); err == nil {
		t.Fatal("an unknown status must be refused")
	}
}

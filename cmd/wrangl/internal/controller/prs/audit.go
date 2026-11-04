package prs

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbtypes"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

// Research citation audit. For a research PR in a project that opted in (project.citation_audit),
// a read-only review run re-fetches every URL the PR adds, checks it resolves and supports the claim
// it is cited for, and reports each one with submit_citation_audit. The PR is not auto-merged unless
// every citation verified, and the result is shown to the owner when they decide.

// Citation statuses an auditor may report.
const (
	CiteVerified    = "verified"    // resolves and supports the claim
	CiteUnsupported = "unsupported" // resolves, but does not support the claim as written
	CiteUnreachable = "unreachable" // did not resolve (4xx/5xx, DNS, timeout, paywall it cannot pass)
)

// CitationResult is one audited URL.
type CitationResult struct {
	URL    string `json:"url"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

// Audit is the stored result for one head commit.
type Audit struct {
	Results []CitationResult `json:"results"`
	Missing []string         `json:"missing,omitempty"` // cited in the diff but not reported on
	Summary string           `json:"summary_md,omitempty"`
	Done    bool             `json:"done"` // false: the audit run ended without reporting
}

// OK is true when every cited URL was reported and verified, and there is at least one result or
// no citations at all.
func (a Audit) OK() bool {
	if !a.Done || len(a.Missing) > 0 {
		return false
	}
	for _, r := range a.Results {
		if r.Status != CiteVerified {
			return false
		}
	}
	return true
}

// Unverified lists what needs the owner's eyes.
func (a Audit) Unverified() []CitationResult {
	var out []CitationResult
	for _, r := range a.Results {
		if r.Status != CiteVerified {
			out = append(out, r)
		}
	}
	for _, m := range a.Missing {
		out = append(out, CitationResult{URL: m, Status: "not checked"})
	}
	return out
}

var urlRe = regexp.MustCompile(`https?://[^\s<>)\]"'` + "`" + `]+`)

// ExtractCitations returns the distinct http(s) URLs on lines a unified diff adds, sorted. Trailing
// punctuation that is not part of a URL is trimmed.
func ExtractCitations(diff string) []string {
	seen := map[string]bool{}
	for _, line := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		for _, u := range urlRe.FindAllString(line, -1) {
			u = strings.TrimRight(u, ".,;:!?*_")
			if u != "" {
				seen[u] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for u := range seen {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// auditWanted: only research PRs in projects that turned the audit on.
func (s *Service) auditWanted(ctx context.Context, proj dbgen.Project, pr dbgen.PullRequest) bool {
	if on, err := s.DB.RQ.GetProjectCitationAudit(ctx, proj.ID); err != nil || on != 1 {
		return false
	}
	t, err := s.DB.RQ.GetTask(ctx, pr.TaskID)
	return err == nil && t.TaskType == dbtypes.TaskResearch
}

// loadAudit returns the stored audit for the PR's current head, if any.
func (s *Service) loadAudit(ctx context.Context, pr dbgen.PullRequest) (Audit, bool) {
	sha, js, err := s.DB.RQ.GetPRAudit(ctx, pr.ID)
	if err != nil || sha != pr.HeadSha || js == "" {
		return Audit{}, false
	}
	var a Audit
	if json.Unmarshal([]byte(js), &a) != nil {
		return Audit{}, false
	}
	return a, true
}

func (s *Service) queueAudit(ctx context.Context, pr dbgen.PullRequest) error {
	_, ghr, err := s.repo(ctx, pr.RepoID)
	if err != nil {
		return err
	}
	diff, err := s.gh(ghr).PRDiff(ctx, ghr, pr.GhNumber)
	if err != nil {
		return err
	}
	urls := ExtractCitations(diff)
	root, _ := s.DB.RQ.GetTask(ctx, pr.TaskID)
	var b strings.Builder
	fmt.Fprintf(&b, "CITATION AUDIT of research pull request #%d (%s) for task %s %q.\n\n", pr.GhNumber, pr.Url, root.Sqid.String, root.Title)
	if len(urls) == 0 {
		b.WriteString("The pull request adds no URLs. Call submit_citation_audit once with an empty citations list and a one-line summary saying so. Do not modify files.\n")
	} else {
		b.WriteString("The report is checked out in your working directory. For EVERY URL below: fetch it, check that it resolves " +
			"(a redirect to the same content is fine), then find where the report cites it and check that the page actually supports " +
			"the claim made there. Do not trust the report's wording, the page decides. Mark each URL verified, unsupported " +
			"(it loads but does not support the claim; say what it says instead) or unreachable (it did not load; say why). " +
			"Then call submit_citation_audit exactly once with a result for every URL and a short markdown summary. " +
			"Do not modify files. If you cannot fetch pages at all, report every URL as unreachable.\n\nURLs added by this pull request:\n")
		for _, u := range urls {
			b.WriteString("- " + u + "\n")
		}
	}
	run, err := s.special(ctx, pr, "review", b.String())
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.audited == nil {
		s.audited = map[int64]string{}
	}
	s.audited[pr.ID] = pr.HeadSha
	s.mu.Unlock()
	s.Log.Info("citation audit queued", "pr", pr.Url, "run", run.ID, "urls", len(urls))
	return nil
}

// SubmitCitationAudit implements the submit_citation_audit tool (review runs).
func (s *Service) SubmitCitationAudit(ctx context.Context, sc runs.Scope, raw json.RawMessage) (any, error) {
	var a struct {
		Citations []CitationResult `json:"citations"`
		Summary   string           `json:"summary_md"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("%w: %v", errs.Invalid, err)
	}
	for _, c := range a.Citations {
		switch c.Status {
		case CiteVerified, CiteUnsupported, CiteUnreachable:
		default:
			return nil, fmt.Errorf("%w: status of %s must be verified, unsupported or unreachable", errs.Invalid, c.URL)
		}
	}
	run, err := s.DB.RQ.GetRun(ctx, sc.RunID)
	if err != nil {
		return nil, err
	}
	if !run.PrID.Valid {
		return nil, fmt.Errorf("%w: this run is not reviewing a PR", errs.Invalid)
	}
	pr, err := s.DB.RQ.GetPR(ctx, run.PrID.Int64)
	if err != nil {
		return nil, err
	}
	// Whatever the diff cites that the auditor did not report on is "not checked", never silently ok.
	_, ghr, err := s.repo(ctx, pr.RepoID)
	if err != nil {
		return nil, err
	}
	diff, err := s.gh(ghr).PRDiff(ctx, ghr, pr.GhNumber)
	if err != nil {
		return nil, err
	}
	reported := map[string]bool{}
	for _, c := range a.Citations {
		reported[c.URL] = true
	}
	res := Audit{Results: a.Citations, Summary: strings.TrimSpace(a.Summary), Done: true}
	for _, u := range ExtractCitations(diff) {
		if !reported[u] {
			res.Missing = append(res.Missing, u)
		}
	}
	js, _ := json.Marshal(res)
	if err := s.DB.Q.SetPRAudit(ctx, pr.HeadSha, string(js), pr.ID); err != nil {
		return nil, err
	}
	s.Log.Info("citation audit submitted", "pr", pr.Url, "ok", res.OK(), "unverified", len(res.Unverified()))
	return map[string]any{"ok": true, "verified": res.OK(), "not_checked": res.Missing, "note": "Audit recorded. You are done; end your turn."}, nil
}

// auditMarkdown renders the audit for the owner's PR question.
func auditMarkdown(a Audit, done bool) string {
	if !done {
		return "\n### Citation audit\n\nThe audit run ended without reporting; nothing was verified.\n"
	}
	var b strings.Builder
	b.WriteString("\n### Citation audit\n\n")
	if a.OK() {
		fmt.Fprintf(&b, "All %d cited URL(s) verified.\n", len(a.Results))
	} else {
		fmt.Fprintf(&b, "**%d citation(s) need a look:**\n\n", len(a.Unverified()))
		for _, r := range a.Unverified() {
			fmt.Fprintf(&b, "- %s: **%s**", r.URL, r.Status)
			if r.Note != "" {
				b.WriteString(" — " + r.Note)
			}
			b.WriteString("\n")
		}
	}
	if a.Summary != "" {
		b.WriteString("\n" + a.Summary + "\n")
	}
	return b.String()
}

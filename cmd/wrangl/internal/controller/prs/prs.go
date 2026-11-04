// Package prs runs the pull-request flow (spec §9): open the PR when a work run pushes,
// compute gates, have an agent review it, then auto-merge or ask the owner; handle
// changes requested (revise runs), webhooks and a reconcile poll.
package prs

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/issuestore"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/projects"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbtypes"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/events"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

// Service is the PR flow.
type Service struct {
	DB            *db.DB
	Author        gh.API // creates PRs
	Reviewer      gh.API // reviews, approves, merges (nil: owner merges by hand)
	Stack         *stack.Service
	Runs          *runs.Service
	WebhookSecret []byte
	// WebhookSecrets, when set, adds the secrets of per-account reviewer Apps.
	WebhookSecrets func() [][]byte
	// HasReviewer reports whether owner has a reviewer App. Nil: a Reviewer, if set, serves every owner.
	HasReviewer func(owner string) bool
	Now         func() time.Time
	Log         *slog.Logger
	Bus         *events.Bus

	mu       sync.Mutex
	reviewed map[int64]string // pr id -> head sha a review run was queued for
	audited  map[int64]string // PR id -> head sha a citation audit was queued for (guarded by mu)
}

// Gates is what the controller checked before considering an auto-merge.
type Gates struct {
	Checks    string   `json:"checks"`
	Lines     int      `json:"lines"`
	MaxLines  int64    `json:"max_lines"`
	Protected []string `json:"protected,omitempty"`
	// OutOfScope lists files a research PR changed outside research/ (the notes repo's report area).
	OutOfScope []string `json:"out_of_scope,omitempty"`
	OK         bool     `json:"ok"`
}

func nstr(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }
func nint(v int64) sql.NullInt64   { return sql.NullInt64{Int64: v, Valid: v != 0} }

// canReview reports whether the reviewer App can act on owner's repositories.
func (s *Service) canReview(owner string) bool {
	if s.Reviewer == nil {
		return false
	}
	return s.HasReviewer == nil || s.HasReviewer(owner)
}

// gh is the client for reading a repository's PRs: the reviewer's when it has one there.
func (s *Service) gh(r gh.Repo) gh.API {
	if s.canReview(r.Owner) {
		return s.Reviewer
	}
	return s.Author
}

func (s *Service) repo(ctx context.Context, id int64) (dbgen.Repo, gh.Repo, error) {
	r, err := s.DB.RQ.GetRepo(ctx, id)
	return r, projects.GHRepo(r), err
}

// --- run finished -------------------------------------------------------------------

// OnRunFinished opens or updates the PR after a work/revise run pushes, and re-evaluates
// after a review run.
func (s *Service) OnRunFinished(ctx context.Context, runID int64) {
	if err := s.onRunFinished(ctx, runID); err != nil {
		s.Log.Error("pr flow after run", "run", runID, "err", err)
	}
}

func (s *Service) onRunFinished(ctx context.Context, runID int64) error {
	if s.Author == nil {
		return nil
	}
	run, err := s.DB.RQ.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	switch run.Kind {
	case "review":
		if run.PrID.Valid {
			return s.Evaluate(ctx, run.PrID.Int64)
		}
		return nil
	case "work", "revise":
	default:
		return nil
	}
	if run.Pushed != 1 || !run.RepoID.Valid {
		return nil
	}
	repo, ghr, err := s.repo(ctx, run.RepoID.Int64)
	if err != nil {
		return err
	}
	pr, err := s.DB.RQ.OpenPRForBranch(ctx, dbgen.OpenPRForBranchParams{RepoID: repo.ID, HeadBranch: run.Branch})
	if errors.Is(err, sql.ErrNoRows) {
		root, err := s.DB.RQ.GetTask(ctx, run.TaskID)
		if err != nil {
			return err
		}
		body, err := s.prBody(ctx, run, root)
		if err != nil {
			return err
		}
		created, err := s.Author.CreatePR(ctx, ghr, run.Branch, repo.DefaultBranch, fmt.Sprintf("[%s] %s", root.Sqid.String, root.Title), body)
		if err != nil {
			return fmt.Errorf("open PR for %s: %w", run.Branch, err)
		}
		pr, err = s.DB.Q.CreatePR(ctx, dbgen.CreatePRParams{TaskID: root.ID, RepoID: repo.ID, RunID: run.ID, GhNumber: created.Number,
			Url: created.URL, HeadBranch: run.Branch, HeadSha: created.HeadSHA})
		if err != nil {
			return err
		}
		s.Log.Info("opened PR", "pr", created.URL, "run", run.ID)
	} else if err != nil {
		return err
	} else {
		// New commits on an open PR (revise or continuation): a fresh review is needed.
		if err := s.DB.Q.UpdatePRHead(ctx, dbgen.UpdatePRHeadParams{HeadSha: run.HeadSha, RunID: run.ID, ID: pr.ID}); err != nil {
			return err
		}
		s.DB.Q.SetPRReview(ctx, dbgen.SetPRReviewParams{ID: pr.ID})
		s.DB.Q.CancelPRQuestions(ctx, pr.ID)
	}
	return s.Evaluate(ctx, pr.ID)
}

func (s *Service) prBody(ctx context.Context, run dbgen.Run, root dbgen.Task) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Work on wrangl task `%s`: **%s**\n\n", root.Sqid.String, root.Title)
	if p := strings.TrimSpace(root.Prompt); p != "" {
		fmt.Fprintf(&b, "%s\n\n", p)
	}
	done, err := s.DB.RQ.CompletedInRun(ctx, nint(run.ID))
	if err != nil {
		return "", err
	}
	if len(done) > 0 {
		b.WriteString("### Completed\n\n")
		for _, d := range done {
			fmt.Fprintf(&b, "- `%s` %s", d.Sqid, d.Title)
			if d.CompletionSummary != "" {
				fmt.Fprintf(&b, ": %s", strings.ReplaceAll(d.CompletionSummary, "\n", " "))
			}
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "\n_Opened by wrangl from run %d (session %s)._\n", run.ID, run.SessionName)
	return b.String(), nil
}

// --- evaluate ---------------------------------------------------------------------------

// globRe converts a protected-path glob (** = any depth, * = within a segment) to a regexp.
func globRe(g string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); i++ {
		switch {
		case strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i++
		case g[i] == '*':
			b.WriteString("[^/]*")
		case g[i] == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(g[i])))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// Protected returns the files matching any of the project's protected globs.
func Protected(files []gh.PRFile, globs []string) []string {
	var res []*regexp.Regexp
	for _, g := range globs {
		res = append(res, globRe(g))
	}
	var hit []string
	for _, f := range files {
		for _, re := range res {
			if re.MatchString(f.Path) {
				hit = append(hit, f.Path)
				break
			}
		}
	}
	return hit
}

// OutsideResearch returns the changed files that are not under research/. A research PR may
// auto-merge only when every file is a report.
func OutsideResearch(files []gh.PRFile) []string {
	var out []string
	for _, f := range files {
		if !strings.HasPrefix(f.Path, "research/") || strings.Contains(f.Path, "..") {
			out = append(out, f.Path)
		}
	}
	return out
}

// Evaluate brings a PR's state up to date and moves it along: gates, review, decision.
func (s *Service) Evaluate(ctx context.Context, prID int64) error {
	defer s.Bus.Publish("pr", fmt.Sprint(prID))
	pr, err := s.DB.RQ.GetPR(ctx, prID)
	if err != nil {
		return err
	}
	if pr.State != "open" {
		return nil
	}
	repo, ghr, err := s.repo(ctx, pr.RepoID)
	if err != nil {
		return err
	}
	live, err := s.gh(ghr).GetPR(ctx, ghr, pr.GhNumber)
	if err != nil {
		return err
	}
	if live.Merged {
		return s.markMerged(ctx, pr, "user_approved") // merged on GitHub directly
	}
	if live.State == "closed" {
		return s.markClosed(ctx, pr)
	}
	if live.HeadSHA != "" && live.HeadSHA != pr.HeadSha {
		s.DB.Q.SetPRHeadSHA(ctx, dbgen.SetPRHeadSHAParams{HeadSha: live.HeadSHA, ID: pr.ID})
		pr.HeadSha = live.HeadSHA
	}
	proj, err := s.projectFor(ctx, pr)
	if err != nil {
		return err
	}
	checks, err := s.gh(ghr).Checks(ctx, ghr, pr.HeadSha)
	if err != nil {
		return err
	}
	g := Gates{Checks: checks, MaxLines: proj.MaxAutoMergeLines}
	if checks != gh.ChecksPending {
		files, err := s.gh(ghr).PRFiles(ctx, ghr, pr.GhNumber)
		if err != nil {
			return err
		}
		for _, f := range files {
			g.Lines += f.Additions + f.Deletions
		}
		var globs []string
		json.Unmarshal([]byte(proj.ProtectedPaths), &globs)
		g.Protected = Protected(files, globs)
		if t, err := s.DB.RQ.GetTask(ctx, pr.TaskID); err == nil && t.TaskType == dbtypes.TaskResearch {
			g.OutOfScope = OutsideResearch(files)
		}
		g.OK = (checks == gh.ChecksSuccess || checks == gh.ChecksNone) && int64(g.Lines) <= g.MaxLines && len(g.Protected) == 0 && len(g.OutOfScope) == 0
	}
	gj, _ := json.Marshal(g)
	if err := s.DB.Q.SetPRGates(ctx, dbgen.SetPRGatesParams{GatesJson: string(gj), Checks: checks, ID: pr.ID}); err != nil {
		return err
	}
	if checks == gh.ChecksPending {
		return nil // a webhook or the poll comes back when CI finishes
	}
	_ = repo

	// Review: one review run per head commit.
	if pr.ReviewedSha != pr.HeadSha {
		s.mu.Lock()
		if s.reviewed == nil {
			s.reviewed = map[int64]string{}
		}
		queued := s.reviewed[pr.ID] == pr.HeadSha
		s.mu.Unlock()
		active, err := s.DB.RQ.ActiveRunForPR(ctx, dbgen.ActiveRunForPRParams{PrID: nint(pr.ID), Kind: "review"})
		if err != nil {
			return err
		}
		if active > 0 {
			return nil
		}
		if !queued {
			return s.queueReview(ctx, pr, g)
		}
		// The review run ended without submitting a review: the owner decides.
	}
	// Research citation audit (opt-in per project): one audit run per head commit, after the review.
	auditing := s.auditWanted(ctx, proj, pr)
	audit, audited := Audit{}, false
	if auditing {
		if audit, audited = s.loadAudit(ctx, pr); !audited {
			active, err := s.DB.RQ.ActiveRunForPR(ctx, dbgen.ActiveRunForPRParams{PrID: nint(pr.ID), Kind: "review"})
			if err != nil {
				return err
			}
			if active > 0 {
				return nil
			}
			s.mu.Lock()
			queued := s.audited[pr.ID] == pr.HeadSha
			s.mu.Unlock()
			if !queued {
				return s.queueAudit(ctx, pr)
			}
			// The audit run ended without reporting: the owner decides, told that nothing was verified.
		}
	}
	ownerGate := false // designs and decisions always wait for the owner
	if t, err := s.DB.RQ.GetTask(ctx, pr.TaskID); err == nil {
		ownerGate = t.TaskType.OwnerGate()
	}
	if proj.AutoMerge == 1 && !ownerGate && g.OK && pr.Risk.String == "low" && pr.ReviewedSha == pr.HeadSha && (!auditing || audit.OK()) && s.autoMergeable(ctx, pr) {
		return s.merge(ctx, pr, "auto_merged", "Auto-merged by wrangl: gates passed and the review rated it low risk.")
	}
	return s.askOwner(ctx, pr, g, auditing, audit, audited)
}

// autoMergeable: the App that opened the PR can merge it, so auto-merge needs no reviewer App.
func (s *Service) autoMergeable(ctx context.Context, pr dbgen.PullRequest) bool {
	_, _, err := s.repo(ctx, pr.RepoID)
	return err == nil && s.Author != nil
}

func (s *Service) projectFor(ctx context.Context, pr dbgen.PullRequest) (dbgen.Project, error) {
	t, err := s.DB.RQ.GetTask(ctx, pr.TaskID)
	if err != nil {
		return dbgen.Project{}, err
	}
	return s.DB.RQ.GetProject(ctx, t.ProjectID)
}

func uuid4() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6], b[8] = (b[6]&0x0f)|0x40, (b[8]&0x3f)|0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func (s *Service) special(ctx context.Context, pr dbgen.PullRequest, kind, prompt string) (dbgen.Run, error) {
	src, err := s.DB.RQ.GetRun(ctx, pr.RunID)
	if err != nil {
		return dbgen.Run{}, err
	}
	n, _ := s.DB.RQ.CountRunsForTask(ctx, pr.TaskID)
	return s.DB.Q.QueueSpecialRun(ctx, dbgen.QueueSpecialRunParams{
		TaskID: pr.TaskID, Kind: kind, Attempt: n + 1, TemplateID: src.TemplateID, RepoID: nint(pr.RepoID),
		Branch: pr.HeadBranch, SessionUuid: uuid4(), PromptOverride: prompt, PrID: nint(pr.ID), ContinueBranch: 1,
	})
}

func (s *Service) queueReview(ctx context.Context, pr dbgen.PullRequest, g Gates) error {
	_, ghr, err := s.repo(ctx, pr.RepoID)
	if err != nil {
		return err
	}
	diff, err := s.gh(ghr).PRDiff(ctx, ghr, pr.GhNumber)
	if err != nil {
		return err
	}
	const max = 60 << 10
	if len(diff) > max {
		diff = diff[:max] + "\n… (diff truncated; read the files in the checkout)"
	}
	root, _ := s.DB.RQ.GetTask(ctx, pr.TaskID)
	var b strings.Builder
	fmt.Fprintf(&b, "Review pull request #%d (%s) for task %s %q.\n\n", pr.GhNumber, pr.Url, root.Sqid.String, root.Title)
	if p := strings.TrimSpace(root.Prompt); p != "" {
		fmt.Fprintf(&b, "Task description:\n%s\n\n", p)
	}
	fmt.Fprintf(&b, "Automated checks: CI %s, %d changed lines", g.Checks, g.Lines)
	if len(g.Protected) > 0 {
		fmt.Fprintf(&b, ", touches protected paths %v", g.Protected)
	}
	b.WriteString(".\n\nThe branch is checked out in your working directory. Read the code, then call the " +
		"submit_review tool exactly once with risk (low, medium or high), a markdown summary, and any concerns. " +
		"Do not modify files.\n\nDiff:\n```diff\n" + diff + "\n```\n")
	run, err := s.special(ctx, pr, "review", b.String())
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.reviewed[pr.ID] = pr.HeadSha
	s.mu.Unlock()
	s.Log.Info("review queued", "pr", pr.Url, "run", run.ID)
	return nil
}

// SubmitReview implements the submit_review tool for review runs.
func (s *Service) SubmitReview(ctx context.Context, sc runs.Scope, raw json.RawMessage) (any, error) {
	var a struct {
		Risk     string   `json:"risk"`
		Summary  string   `json:"summary_md"`
		Concerns []string `json:"concerns"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("%w: %v", errs.Invalid, err)
	}
	if a.Risk != "low" && a.Risk != "medium" && a.Risk != "high" {
		return nil, fmt.Errorf("%w: risk must be low, medium or high", errs.Invalid)
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
	summary := strings.TrimSpace(a.Summary)
	if len(a.Concerns) > 0 {
		summary += "\n\n**Concerns**\n"
		for _, c := range a.Concerns {
			summary += "- " + c + "\n"
		}
	}
	if err := s.DB.Q.SetPRReview(ctx, dbgen.SetPRReviewParams{Risk: nstr(a.Risk), Summary: summary, ReviewedSha: pr.HeadSha, ID: pr.ID}); err != nil {
		return nil, err
	}
	s.Log.Info("review submitted", "pr", pr.Url, "risk", a.Risk)
	return map[string]any{"ok": true, "note": "Review recorded. You are done; end your turn."}, nil
}

func (s *Service) askOwner(ctx context.Context, pr dbgen.PullRequest, g Gates, auditing bool, audit Audit, audited bool) error {
	n, err := s.DB.RQ.OpenQuestionForPR(ctx, pr.ID)
	if err != nil || n > 0 {
		return err
	}
	pr, _ = s.DB.RQ.GetPR(ctx, pr.ID)
	risk := pr.Risk.String
	if risk == "" {
		risk = "not reviewed (the review run did not submit one)"
	}
	var d strings.Builder
	fmt.Fprintf(&d, "%s\n\n**Risk:** %s  \n**CI:** %s  \n**Changed lines:** %d (auto-merge limit %d)\n", pr.Url, risk, g.Checks, g.Lines, g.MaxLines)
	if len(g.Protected) > 0 {
		fmt.Fprintf(&d, "**Protected paths:** %s\n", strings.Join(g.Protected, ", "))
	}
	if pr.ReviewSummaryMd != "" {
		fmt.Fprintf(&d, "\n### Review\n\n%s\n", pr.ReviewSummaryMd)
	}
	if auditing {
		d.WriteString(auditMarkdown(audit, audited))
	}
	payload, _ := json.Marshal(map[string]int64{"pr_id": pr.ID})
	_, err = s.Runs.NewQuestion(ctx, dbgen.CreateQuestionParams{TaskID: pr.TaskID, Kind: "pr_review",
		Prompt: fmt.Sprintf("Review PR #%d %q", pr.GhNumber, strings.TrimPrefix(pr.HeadBranch, "wrangl/")), DetailMd: d.String(),
		AllowFreeText: 1, PayloadJson: string(payload)}, []proto.QuestionOption{
		{Label: "Approve & merge", Value: "approve"},
		{Label: "Request changes", Value: "request_changes", Description: "say what to change in the text"},
		{Label: "Close", Value: "close"},
	})
	return err
}

// --- decisions -----------------------------------------------------------------------

func (s *Service) merge(ctx context.Context, pr dbgen.PullRequest, decision, note string) error {
	_, ghr, err := s.repo(ctx, pr.RepoID)
	if err != nil {
		return err
	}
	// One App is enough: it merges. An approving review is only posted when the owner set up the
	// optional reviewer App (needed only where GitHub requires approvals before a merge).
	api, approve := s.Author, s.canReview(ghr.Owner)
	if approve {
		api = s.Reviewer
		if err := api.Review(ctx, ghr, pr.GhNumber, "APPROVE", note); err != nil {
			return fmt.Errorf("approve PR #%d: %w", pr.GhNumber, err)
		}
	}
	root, _ := s.DB.RQ.GetTask(ctx, pr.TaskID)
	if err := api.Merge(ctx, ghr, pr.GhNumber, fmt.Sprintf("[%s] %s (#%d)", root.Sqid.String, root.Title, pr.GhNumber)); err != nil {
		var he *gh.HTTPError
		if !approve && errors.As(err, &he) && (he.Status == 405 || he.Status == 409 || he.Status == 422) {
			return fmt.Errorf("GitHub would not merge PR #%d (%v). If %s/%s requires approving reviews, add the wrangl App to its ruleset's bypass list, or set up the optional reviewer App in Settings > GitHub accounts", pr.GhNumber, he.Body, ghr.Owner, ghr.Name)
		}
		return fmt.Errorf("merge PR #%d: %w", pr.GhNumber, err)
	}
	return s.markMerged(ctx, pr, decision)
}

func (s *Service) markMerged(ctx context.Context, pr dbgen.PullRequest, decision string) error {
	ids, err := s.DB.RQ.RunsForPR(ctx, dbgen.RunsForPRParams{RepoID: nint(pr.RepoID), Branch: pr.HeadBranch})
	if err != nil {
		return err
	}
	if err := s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		if err := q.ClosePRRow(ctx, dbgen.ClosePRRowParams{State: "merged", Decision: nstr(decision), ID: pr.ID}); err != nil {
			return err
		}
		for _, id := range ids {
			if err := q.MarkRunTasksDone(ctx, nint(id)); err != nil {
				return err
			}
		}
		return q.CancelPRQuestions(ctx, pr.ID)
	}); err != nil {
		return err
	}
	for _, id := range ids {
		done, _ := s.DB.RQ.CompletedInRun(ctx, nint(id))
		for _, d := range done {
			s.Stack.Recompute(ctx, d.Sqid)
		}
	}
	s.Log.Info("PR merged", "pr", pr.Url, "decision", decision)
	return s.continueBlocked(ctx)
}

func (s *Service) markClosed(ctx context.Context, pr dbgen.PullRequest) error {
	ids, err := s.DB.RQ.RunsForPR(ctx, dbgen.RunsForPRParams{RepoID: nint(pr.RepoID), Branch: pr.HeadBranch})
	if err != nil {
		return err
	}
	var reopened []string
	for _, id := range ids {
		done, _ := s.DB.RQ.CompletedInRun(ctx, nint(id))
		for _, d := range done {
			reopened = append(reopened, d.Sqid)
		}
	}
	if err := s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		if err := q.ClosePRRow(ctx, dbgen.ClosePRRowParams{State: "closed", Decision: nstr("closed"), ID: pr.ID}); err != nil {
			return err
		}
		for _, id := range ids {
			if err := q.ReopenRunTasks(ctx, nint(id)); err != nil {
				return err
			}
		}
		return q.CancelPRQuestions(ctx, pr.ID)
	}); err != nil {
		return err
	}
	for _, sq := range reopened {
		s.Stack.Recompute(ctx, sq)
	}
	s.Log.Info("PR closed without merging; its tasks were reopened", "pr", pr.Url)
	return nil
}

// Approve approves and merges a PR (owner decision).
func (s *Service) Approve(ctx context.Context, prID int64) error {
	pr, err := s.openPR(ctx, prID)
	if err != nil {
		return err
	}
	return s.merge(ctx, pr, "user_approved", "Approved by the owner via wrangl.")
}

// RequestChanges queues a revise run on the PR's branch with the owner's instructions.
func (s *Service) RequestChanges(ctx context.Context, prID int64, text string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("%w: say what to change", errs.Invalid)
	}
	pr, err := s.openPR(ctx, prID)
	if err != nil {
		return err
	}
	if _, ghr, _ := s.repo(ctx, pr.RepoID); s.canReview(ghr.Owner) {
		if err := s.Reviewer.Review(ctx, ghr, pr.GhNumber, "REQUEST_CHANGES", text); err != nil {
			s.Log.Warn("posting change request on GitHub", "err", err)
		}
	}
	prompt := fmt.Sprintf("The owner requested changes on pull request #%d (%s):\n\n%s\n", pr.GhNumber, pr.Url, text)
	if pr.ReviewSummaryMd != "" {
		prompt += "\nThe earlier review said:\n" + pr.ReviewSummaryMd + "\n"
	}
	prompt += "\nThe PR branch is checked out. Make the requested changes and commit them; end your turn when done.\n"
	run, err := s.special(ctx, pr, "revise", prompt)
	if err != nil {
		return err
	}
	s.DB.Q.SetPRDecision(ctx, dbgen.SetPRDecisionParams{Decision: nstr("changes_requested"), ID: pr.ID})
	s.DB.Q.CancelPRQuestions(ctx, pr.ID)
	s.Log.Info("revise queued", "pr", pr.Url, "run", run.ID)
	return nil
}

// Close closes a PR without merging and reopens its tasks.
func (s *Service) Close(ctx context.Context, prID int64) error {
	pr, err := s.openPR(ctx, prID)
	if err != nil {
		return err
	}
	_, ghr, err := s.repo(ctx, pr.RepoID)
	if err != nil {
		return err
	}
	if err := s.gh(ghr).ClosePR(ctx, ghr, pr.GhNumber); err != nil {
		return err
	}
	return s.markClosed(ctx, pr)
}

func (s *Service) openPR(ctx context.Context, id int64) (dbgen.PullRequest, error) {
	pr, err := s.DB.RQ.GetPR(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return pr, fmt.Errorf("%w: PR %d", errs.NotFound, id)
	} else if err != nil {
		return pr, err
	}
	if pr.State != "open" {
		return pr, fmt.Errorf("%w: PR %d is %s", errs.Conflict, id, pr.State)
	}
	return pr, nil
}

// AnswerPR handles an answered pr_review question.
func (s *Service) AnswerPR(ctx context.Context, q dbgen.Question, value, text string) error {
	var p struct {
		PrID int64 `json:"pr_id"`
	}
	json.Unmarshal([]byte(q.PayloadJson), &p)
	switch value {
	case "approve":
		return s.Approve(ctx, p.PrID)
	case "request_changes":
		return s.RequestChanges(ctx, p.PrID, text)
	case "close":
		return s.Close(ctx, p.PrID)
	}
	return nil
}

// continueBlocked re-queues blocked work whose outside precursors are now done. If the
// blocked run's PR is still open the new run continues its branch.
func (s *Service) continueBlocked(ctx context.Context) error {
	blocked, err := s.DB.RQ.LatestBlockedRuns(ctx)
	if err != nil {
		return err
	}
	for _, r := range blocked {
		if _, err := s.DB.RQ.NextReadyLeaf(ctx, dbgen.NextReadyLeafParams{RootID: r.TaskID}); errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return err
		}
		if pr, err := s.DB.RQ.OpenPRForBranch(ctx, dbgen.OpenPRForBranchParams{RepoID: r.RepoID.Int64, HeadBranch: r.Branch}); err == nil {
			n, _ := s.DB.RQ.CountRunsForTask(ctx, r.TaskID)
			run, err := s.DB.Q.QueueSpecialRun(ctx, dbgen.QueueSpecialRunParams{TaskID: r.TaskID, Kind: "work", Attempt: n + 1,
				TemplateID: r.TemplateID, RepoID: r.RepoID, Branch: r.Branch, SessionUuid: uuid4(), PrID: nint(pr.ID), ContinueBranch: 1})
			if err != nil {
				return err
			}
			s.Log.Info("continuing blocked work on its PR branch", "run", run.ID, "branch", r.Branch)
			continue
		}
		t, err := s.DB.RQ.GetTask(ctx, r.TaskID)
		if err != nil {
			return err
		}
		if run, err := s.Stack.Queue(ctx, t.Sqid.String); err == nil {
			s.Log.Info("re-queued blocked work", "run", run.ID, "task", t.Sqid.String)
		}
	}
	return nil
}

// --- webhooks & reconcile ---------------------------------------------------------------

func (s *Service) secrets() [][]byte {
	var out [][]byte
	if len(s.WebhookSecret) > 0 {
		out = append(out, s.WebhookSecret)
	}
	if s.WebhookSecrets != nil {
		out = append(out, s.WebhookSecrets()...)
	}
	return out
}

// Webhook handles POST /gh/webhook (reviewer App): HMAC-verified, deduplicated.
func (s *Service) Webhook(w http.ResponseWriter, r *http.Request) {
	secrets := s.secrets()
	if len(secrets) == 0 {
		http.Error(w, "webhooks are not configured (github.reviewer_app.webhook_secret)", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 25<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ok := false
	for _, sec := range secrets {
		mac := hmac.New(sha256.New, sec)
		mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		ok = ok || hmac.Equal([]byte(want), []byte(r.Header.Get("X-Hub-Signature-256")))
	}
	if !ok {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	id := r.Header.Get("X-GitHub-Delivery")
	if id != "" {
		n, err := s.DB.Q.RecordDelivery(r.Context(), dbgen.RecordDeliveryParams{ID: id, ReceivedAt: s.Now().UTC().Format(time.RFC3339)})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if n == 0 {
			w.WriteHeader(http.StatusOK) // duplicate delivery
			return
		}
	}
	event := r.Header.Get("X-GitHub-Event")
	w.WriteHeader(http.StatusAccepted)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.handleEvent(ctx, event, body); err != nil {
			s.Log.Error("webhook", "event", event, "delivery", id, "err", err)
		}
	}()
}

func (s *Service) handleEvent(ctx context.Context, event string, body []byte) error {
	var p struct {
		Action     string `json:"action"`
		CheckSuite struct {
			HeadSHA string `json:"head_sha"`
		} `json:"check_suite"`
		CheckRun struct {
			HeadSHA string `json:"head_sha"`
		} `json:"check_run"`
		PullRequest struct {
			Number int64 `json:"number"`
		} `json:"pull_request"`
		Issue struct {
			Number  int64  `json:"number"`
			Title   string `json:"title"`
			Body    string `json:"body"`
			State   string `json:"state"`
			HTMLURL string `json:"html_url"`
			User    struct {
				Login string `json:"login"`
			} `json:"user"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
			CreatedAt   string          `json:"created_at"`
			UpdatedAt   string          `json:"updated_at"`
			Comments    int             `json:"comments"`
			PullRequest json.RawMessage `json:"pull_request"`
		} `json:"issue"`
		Repository struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	owner, name := p.Repository.Owner.Login, p.Repository.Name
	switch event {
	case "check_suite", "check_run":
		sha := p.CheckSuite.HeadSHA + p.CheckRun.HeadSHA
		prs, err := s.DB.RQ.OpenPRsByHeadSHA(ctx, sha)
		if err != nil {
			return err
		}
		for _, pr := range prs {
			if err := s.Evaluate(ctx, pr.ID); err != nil {
				return err
			}
		}
	case "pull_request", "pull_request_review":
		pr, err := s.DB.RQ.PRByNumber(ctx, dbgen.PRByNumberParams{Owner: owner, Name: name, GhNumber: p.PullRequest.Number})
		if errors.Is(err, sql.ErrNoRows) {
			return nil // not a wrangl PR
		} else if err != nil {
			return err
		}
		return s.Evaluate(ctx, pr.ID)
	case "issues":
		repos, err := s.DB.RQ.ReposByName(ctx, dbgen.ReposByNameParams{Owner: owner, Name: name})
		if err != nil {
			return err
		}
		if len(p.Issue.PullRequest) > 0 {
			return nil // pull requests are not issues here
		}
		i := gh.Issue{Number: p.Issue.Number, Title: p.Issue.Title, Body: p.Issue.Body, State: p.Issue.State, URL: p.Issue.HTMLURL,
			Author: p.Issue.User.Login, CreatedAt: p.Issue.CreatedAt, UpdatedAt: p.Issue.UpdatedAt, Comments: p.Issue.Comments}
		for _, l := range p.Issue.Labels {
			i.Labels = append(i.Labels, l.Name)
		}
		if p.Action == "deleted" || p.Action == "transferred" {
			// The issue is gone from (or left) this repo: drop our row. Tasks keep existing, unlinked.
			// A transferred issue reappears under its new repo at the next sync if that repo is linked.
			for _, r := range repos {
				if _, err := issuestore.Delete(ctx, s.DB, r.ID, p.Issue.Number); err != nil {
					return err
				}
			}
			return nil
		}
		for _, r := range repos {
			// Reconciler: only a newer updated_at changes the row, so echoes of our own writes
			// and duplicate or reordered deliveries are ignored.
			if _, _, err := issuestore.Save(ctx, s.DB, r.ID, i, s.Now()); err != nil {
				return err
			}
		}
	}
	return nil
}

// Reconcile re-evaluates every open PR (missed webhooks, CI finishing, merges on GitHub).
func (s *Service) Reconcile(ctx context.Context) {
	prs, err := s.DB.RQ.OpenPRs(ctx)
	if err != nil {
		s.Log.Error("reconcile PRs", "err", err)
		return
	}
	for _, pr := range prs {
		if err := s.Evaluate(ctx, pr.ID); err != nil {
			s.Log.Warn("reconcile PR", "pr", pr.Url, "err", err)
		}
	}
}

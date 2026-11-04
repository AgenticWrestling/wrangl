package issuetools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/issuestore"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

// Limits on what an agent may send. GitHub's own limit for a body is 65 536 characters; the
// footer we append needs room.
const (
	maxBody   = 65000
	maxTitle  = 256
	maxLabels = 10
)

type writeArgs struct {
	Project     string          `json:"project"`
	Repo        string          `json:"repo"`
	Ref         string          `json:"ref"`
	Number      int64           `json:"number"`
	Title       *string         `json:"title"`
	Body        *string         `json:"body"`
	Reason      string          `json:"reason"`
	StateReason string          `json:"state_reason"`
	Labels      json.RawMessage `json:"labels"`
}

func parseWrite(raw json.RawMessage) (writeArgs, error) {
	var a writeArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return a, fmt.Errorf("%w: arguments: %v", errs.Invalid, err)
		}
	}
	return a, nil
}

func (a writeArgs) base() args {
	return args{Project: a.Project, Repo: a.Repo, Ref: a.Ref, Number: a.Number}
}

// writer resolves the target of a write: the caller's kind may write, the repo is one linked to
// the project, GitHub is configured. It spends one live-call token.
func (s *Service) writer(ctx context.Context, sc runs.Scope, a writeArgs, needNumber bool) (target, *dbgen.Repo, int64, error) {
	switch sc.Kind {
	case "work", "revise", "user":
	default:
		return target{}, nil, 0, fmt.Errorf("%w: %s runs cannot change GitHub issues", errs.Forbidden, sc.Kind)
	}
	t, err := s.scope(ctx, sc, a.base())
	if err != nil {
		return t, nil, 0, err
	}
	var r *dbgen.Repo
	var n int64
	if needNumber {
		r, n, err = t.repoAndNumber(a.base())
	} else {
		r, err = t.pick(a.Repo, false)
	}
	if err != nil {
		return t, nil, 0, err
	}
	return t, r, n, nil
}

// begin takes the live-call token; call it only after every check that needs no GitHub.
func (s *Service) begin(t target) error {
	if s.GH == nil {
		return errNoGitHub
	}
	return s.spend(t.project)
}

func mapWriteErr(r dbgen.Repo, n int64, err error) error {
	if errors.Is(err, gh.ErrIssuePermission) {
		return fmt.Errorf("%w: GitHub refused the write to %s/%s: the wrangl GitHub App needs the issues: write permission, "+
			"and the owner must accept it on the installation. See docs/wrangl/INSTALL.md, \"Upgrading existing Apps to issues: write\" (%v)",
			errs.Forbidden, r.Owner, r.Name, err)
	}
	return mapGH(r, n, err)
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// footer names the run and task so people on GitHub can tell who wrote it.
func (s *Service) footer(ctx context.Context, sc runs.Scope) string {
	if sc.RunID == 0 {
		return ""
	}
	sqid := ""
	if run, err := s.DB.RQ.GetRun(ctx, sc.RunID); err == nil {
		id := run.TaskID
		if run.CurrentTaskID.Valid {
			id = run.CurrentTaskID.Int64
		}
		if t, err := s.DB.RQ.GetTask(ctx, id); err == nil {
			sqid = t.Sqid.String
		}
	}
	if sqid == "" {
		return fmt.Sprintf("\n\n— wrangl run %d", sc.RunID)
	}
	return fmt.Sprintf("\n\n— wrangl run %d, task %s", sc.RunID, sqid)
}

func (s *Service) audit(sc runs.Scope, op, ref, url string) {
	s.log().Info("issue write", "actor", string(sc.Actor), "op", op, "ref", ref, "url", url)
}

// cache write-through: store GitHub's answer. A local failure is logged and reported as
// cached:false; the webhook or the next sync repairs the row.
func (s *Service) cache(ctx context.Context, r dbgen.Repo, i gh.Issue) (issuestore.Row, bool) {
	if _, _, err := issuestore.Save(ctx, s.DB, r.ID, i, s.now()); err == nil {
		if row, err := issuestore.Get(ctx, s.DB, r.ID, i.Number); err == nil {
			return row, true
		}
	} else {
		s.log().Error("issue write: cache failed", "repo", r.Owner+"/"+r.Name, "number", i.Number, "err", err)
	}
	return issuestore.Row{Owner: r.Owner, Repo: r.Name, Number: i.Number, Title: i.Title, Body: i.Body, State: i.State, URL: i.URL,
		Author: i.Author, Labels: i.Labels, GhUpdatedAt: issuestore.NormTime(i.UpdatedAt), CommentCount: int64(i.Comments)}, false
}

func (s *Service) issueResult(row issuestore.Row, cached bool) map[string]any {
	m := s.summary(row, "")
	m["body"] = Wrap(ref(row), "body", row.Body)
	m["cached"] = cached
	m["untrusted"], m["notice"] = true, Notice
	return m
}

func checkText(field, v string, max int) error {
	if utf8.RuneCountInString(v) > max {
		return fmt.Errorf("%w: %s is longer than %d characters", errs.Invalid, field, max)
	}
	return nil
}

// Comment implements issue_comment.
func (s *Service) Comment(ctx context.Context, sc runs.Scope, raw json.RawMessage) (any, error) {
	a, err := parseWrite(raw)
	if err != nil {
		return nil, err
	}
	t, r, n, err := s.writer(ctx, sc, a, true)
	if err != nil {
		return nil, err
	}
	if a.Body == nil || strings.TrimSpace(*a.Body) == "" {
		return nil, fmt.Errorf("%w: body is required", errs.Invalid)
	}
	if err := checkText("body", *a.Body, maxBody); err != nil {
		return nil, err
	}
	if err := s.begin(t); err != nil {
		return nil, err
	}
	c, err := s.GH.CreateComment(ctx, ghRepo(*r), n, *a.Body+s.footer(ctx, sc))
	if err != nil {
		return nil, mapWriteErr(*r, n, err)
	}
	refStr := fmt.Sprintf("%s/%s#%d", r.Owner, r.Name, n)
	s.audit(sc, "issue_comment", refStr, c.URL)
	cached := false
	if i, err := s.GH.GetIssue(ctx, ghRepo(*r), n); err == nil {
		_, cached = s.cache(ctx, *r, i)
	}
	return map[string]any{"ref": refStr, "comment_id": c.ID, "url": c.URL, "cached": cached}, nil
}

// Create implements issue_create.
func (s *Service) Create(ctx context.Context, sc runs.Scope, raw json.RawMessage) (any, error) {
	a, err := parseWrite(raw)
	if err != nil {
		return nil, err
	}
	t, r, _, err := s.writer(ctx, sc, a, false)
	if err != nil {
		return nil, err
	}
	if a.Title == nil || strings.TrimSpace(*a.Title) == "" {
		return nil, fmt.Errorf("%w: title is required", errs.Invalid)
	}
	if err := checkText("title", *a.Title, maxTitle); err != nil {
		return nil, err
	}
	body := ""
	if a.Body != nil {
		body = *a.Body
	}
	if err := checkText("body", body, maxBody); err != nil {
		return nil, err
	}
	var labels []string
	if len(a.Labels) > 0 {
		if err := json.Unmarshal(a.Labels, &labels); err != nil {
			return nil, fmt.Errorf("%w: labels must be a list of names", errs.Invalid)
		}
	}
	if len(labels) > maxLabels {
		return nil, fmt.Errorf("%w: at most %d labels", errs.Invalid, maxLabels)
	}
	if err := s.begin(t); err != nil {
		return nil, err
	}
	i, err := s.GH.CreateIssue(ctx, ghRepo(*r), strings.TrimSpace(*a.Title), body+s.footer(ctx, sc), labels)
	if err != nil {
		return nil, mapWriteErr(*r, 0, err)
	}
	row, cached := s.cache(ctx, *r, i)
	s.audit(sc, "issue_create", ref(row), i.URL)
	return s.issueResult(row, cached), nil
}

// Edit implements issue_edit.
func (s *Service) Edit(ctx context.Context, sc runs.Scope, raw json.RawMessage) (any, error) {
	a, err := parseWrite(raw)
	if err != nil {
		return nil, err
	}
	t, r, n, err := s.writer(ctx, sc, a, true)
	if err != nil {
		return nil, err
	}
	e := gh.IssueEdit{Title: a.Title, Body: a.Body}
	if a.Title != nil {
		if strings.TrimSpace(*a.Title) == "" {
			return nil, fmt.Errorf("%w: title cannot be empty", errs.Invalid)
		}
		if err := checkText("title", *a.Title, maxTitle); err != nil {
			return nil, err
		}
	}
	if a.Body != nil {
		if err := checkText("body", *a.Body, maxBody); err != nil {
			return nil, err
		}
	}
	if len(a.Labels) > 0 {
		var l struct{ Add, Remove []string }
		if err := json.Unmarshal(a.Labels, &l); err != nil {
			return nil, fmt.Errorf("%w: labels must be {add: [...], remove: [...]}", errs.Invalid)
		}
		if len(l.Add) > maxLabels || len(l.Remove) > maxLabels {
			return nil, fmt.Errorf("%w: at most %d labels", errs.Invalid, maxLabels)
		}
		e.AddLabels, e.RemoveLabels = l.Add, l.Remove
	}
	if e.Title == nil && e.Body == nil && len(e.AddLabels) == 0 && len(e.RemoveLabels) == 0 {
		return nil, fmt.Errorf("%w: nothing to change (give title, body or labels)", errs.Invalid)
	}
	if err := s.begin(t); err != nil {
		return nil, err
	}
	i, err := s.GH.EditIssue(ctx, ghRepo(*r), n, e)
	if err != nil {
		return nil, mapWriteErr(*r, n, err)
	}
	row, cached := s.cache(ctx, *r, i)
	s.audit(sc, "issue_edit", ref(row), i.URL)
	return s.issueResult(row, cached), nil
}

// closePayload is the approval question's payload for issue_close.
type closePayload struct {
	Op          string `json:"op"`
	RunID       int64  `json:"run_id"`
	ProjectID   int64  `json:"project_id"`
	Repo        string `json:"repo"`
	Number      int64  `json:"number"`
	Reason      string `json:"reason,omitempty"`
	StateReason string `json:"state_reason,omitempty"`
}

// fence quotes text in a markdown code block that the text cannot close.
func fence(text string) string {
	text = strings.ReplaceAll(text, "\r", "")
	f := "```"
	for strings.Contains(text, f) {
		f += "`"
	}
	return f + "\n" + text + "\n" + f
}

// Close implements issue_close: an agent's request needs the owner's approval; the owner acts directly.
func (s *Service) Close(ctx context.Context, sc runs.Scope, raw json.RawMessage) (any, error) {
	a, err := parseWrite(raw)
	if err != nil {
		return nil, err
	}
	switch a.StateReason {
	case "", "completed", "not_planned":
	default:
		return nil, fmt.Errorf("%w: state_reason must be completed or not_planned", errs.Invalid)
	}
	t, r, n, err := s.writer(ctx, sc, a, true)
	if err != nil {
		return nil, err
	}
	if sc.RunID == 0 {
		return s.doClose(ctx, sc, t, *r, n, a.StateReason)
	}
	if s.Runs == nil {
		return nil, fmt.Errorf("%w: approvals are not available", errs.Invalid)
	}
	if err := checkText("reason", a.Reason, 2000); err != nil {
		return nil, err
	}
	row, _, err := s.issue(ctx, t, *r, n, false) // the owner sees the real title
	if err != nil {
		return nil, err
	}
	refStr := ref(row)
	detail := "Issue " + refStr + " (" + row.URL + "), title:\n\n" + fence(row.Title)
	if a.Reason != "" {
		detail += "\n\nAgent's reason:\n\n" + fence(a.Reason)
	}
	qid, err := s.Runs.RequestApproval(ctx, sc.RunID,
		fmt.Sprintf("The agent on run %d wants to close the GitHub issue %s.", sc.RunID, refStr), detail,
		closePayload{Op: "issue_close", RunID: sc.RunID, ProjectID: t.project, Repo: r.Owner + "/" + r.Name, Number: n,
			Reason: a.Reason, StateReason: a.StateReason})
	if err != nil {
		return nil, err
	}
	return map[string]any{"pending_approval": true, "question_id": qid,
		"note": "The owner must approve closing this issue; carry on with other work."}, nil
}

func (s *Service) doClose(ctx context.Context, sc runs.Scope, t target, r dbgen.Repo, n int64, stateReason string) (any, error) {
	if err := s.begin(t); err != nil {
		return nil, err
	}
	i, err := s.GH.CloseIssue(ctx, ghRepo(r), n, stateReason)
	if err != nil {
		return nil, mapWriteErr(r, n, err)
	}
	row, cached := s.cache(ctx, r, i)
	s.audit(sc, "issue_close", ref(row), i.URL)
	return s.issueResult(row, cached), nil
}

// CloseApproved runs a close the owner approved. The repo must still be linked to the project
// the request came from.
func (s *Service) CloseApproved(ctx context.Context, payload json.RawMessage) error {
	var p closePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return err
	}
	sc := runs.Scope{RunID: p.RunID, Kind: "work", ProjectID: p.ProjectID, Actor: runs.ActorFor(p.RunID)}
	t, err := s.scope(ctx, sc, args{})
	if err != nil {
		return err
	}
	r, err := t.pick(p.Repo, false)
	if err != nil {
		return err
	}
	_, err = s.doClose(ctx, sc, t, *r, p.Number, p.StateReason)
	return err
}

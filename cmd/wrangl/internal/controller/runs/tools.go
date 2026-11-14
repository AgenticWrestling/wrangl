package runs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/reporesolve"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stacktools"
)

// SystemQuestionTool is the daemon-only tool the runner uses to ask the owner what to do
// when an agent stops without completing its task. It is not in the MCP catalogue, and
// the daemon refuses it from the agent's socket.
const SystemQuestionTool = "_system_question"

// Scope is who is calling and what they may touch.
type Scope struct {
	RunID     int64 // 0 = the owner (remote MCP)
	Kind      string
	RootID    int64 // assignment root; 0 = no subtree restriction
	ProjectID int64 // 0 = any project
	Actor     stack.Actor
}

// ActorFor is the actor of a run (the owner for run 0).
func ActorFor(runID int64) stack.Actor { return stackActor(runID) }

func stackActor(runID int64) stack.Actor {
	if runID == 0 {
		return stack.User
	}
	return stack.AgentActor(runID)
}

// ScopeForRun derives the scope of a run's agent.
func (s *Service) ScopeForRun(ctx context.Context, runID int64) (Scope, error) {
	run, err := s.DB.RQ.GetRun(ctx, runID)
	if err != nil {
		return Scope{}, err
	}
	root, err := s.DB.RQ.GetTask(ctx, run.TaskID)
	if err != nil {
		return Scope{}, err
	}
	return Scope{RunID: runID, Kind: run.Kind, RootID: root.ID, ProjectID: root.ProjectID, Actor: stack.AgentActor(runID)}, nil
}

// UserScope is the owner acting through the remote MCP endpoint.
func UserScope() Scope { return Scope{Kind: "user", Actor: stack.User} }

func (s *Service) task(ctx context.Context, sc Scope, sqid string) (dbgen.Task, error) {
	t, err := s.Stack.Task(ctx, s.DB.RQ, sqid)
	if err != nil {
		return t, err
	}
	if sc.ProjectID != 0 && t.ProjectID != sc.ProjectID {
		return t, fmt.Errorf("%w: task %q", errs.NotFound, sqid) // other projects are invisible
	}
	return t, nil
}

// writable returns the task if it is inside the caller's subtree.
func (s *Service) writable(ctx context.Context, sc Scope, sqid string) (dbgen.Task, error) {
	t, err := s.task(ctx, sc, sqid)
	if err != nil || sc.RootID == 0 {
		return t, err
	}
	in, err := s.DB.RQ.IsAncestorOrSelf(ctx, dbgen.IsAncestorOrSelfParams{NodeID: t.ID, AncestorID: sc.RootID})
	if err != nil {
		return t, err
	}
	if in != 1 {
		return t, fmt.Errorf("%w: %s is outside your assignment; you may read it but not change it", errs.Forbidden, sqid)
	}
	return t, nil
}

// Writable is writable for other services: the task, if it is inside the caller's subtree.
func (s *Service) Writable(ctx context.Context, sc Scope, sqid string) (dbgen.Task, error) {
	return s.writable(ctx, sc, sqid)
}

type posArg struct {
	First  bool   `json:"first"`
	After  string `json:"after"`
	Before string `json:"before"`
}

func (p *posArg) pos() stack.Position {
	if p == nil {
		return stack.Position{}
	}
	return stack.Position{First: p.First, After: p.After, Before: p.Before}
}

func taskJSON(t dbgen.Task) map[string]any {
	return map[string]any{"sqid": t.Sqid.String, "title": t.Title, "state": t.State, "type": string(t.TaskType)}
}

// ExecTool runs one work-stack tool for a caller.
func (s *Service) ExecTool(ctx context.Context, sc Scope, tool string, raw json.RawMessage) (any, error) {
	if tool != SystemQuestionTool && !stacktools.Allowed(sc.Kind)[tool] {
		return nil, fmt.Errorf("%w: %s is not available to %s runs", errs.Forbidden, tool, sc.Kind)
	}
	var a struct {
		Sqid, Parent, Title, Prompt, Precursor, Summary, Reason, State, Text, Project string
		Needs                                                                         []string
		Need                                                                          map[string]string
		Position                                                                      *posArg
		Percent                                                                       *int
		Limit                                                                         *int
		Type, Archived                                                                string
		TargetDate                                                                    *string `json:"target_date"`
		Repo                                                                          *string `json:"repo"` // task_create / task_update: owner/name; "" clears
		// ask_question
		DetailMD      string                 `json:"detail_md"`
		AllowFreeText bool                   `json:"allow_free_text"`
		Options       []proto.QuestionOption `json:"options"`
		// submit_review / propose_split are decoded by their handlers
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("%w: arguments: %v", errs.Invalid, err)
		}
	}
	need := func(v, name string) error {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%w: %s is required", errs.Invalid, name)
		}
		return nil
	}
	switch tool {
	case "project_list":
		filter := db.ProjectsActive
		switch a.Archived {
		case "", "active":
		case "archived", "all":
			filter = a.Archived
		default:
			return nil, fmt.Errorf("%w: archived must be active, archived or all", errs.Invalid)
		}
		ps, err := s.DB.RQ.ListProjects(ctx, filter)
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, p := range ps {
			m := map[string]any{"slug": p.Slug, "name": p.Name, "auto_merge": p.AutoMerge == 1, "auto_queue": p.AutoQueue == 1, "archived": p.ArchivedAt.Valid}
			if p.DefaultTemplateID.Valid {
				if t, err := s.DB.RQ.GetTemplate(ctx, p.DefaultTemplateID.Int64); err == nil {
					m["template"] = t.Name
				}
			}
			out = append(out, m)
		}
		return out, nil

	case "task_queue":
		if err := need(a.Sqid, "sqid"); err != nil {
			return nil, err
		}
		if _, err := s.task(ctx, sc, a.Sqid); err != nil {
			return nil, err
		}
		run, err := s.Stack.Queue(ctx, a.Sqid)
		if err != nil {
			return nil, err
		}
		return map[string]any{"run_id": run.ID, "state": run.State}, nil

	case "run_list":
		lim := int64(20)
		if a.Limit != nil && *a.Limit > 0 && *a.Limit <= 200 {
			lim = int64(*a.Limit)
		}
		rows, err := s.DB.RQ.ListRuns(ctx, dbgen.ListRunsParams{State: a.State, Project: a.Project, Lim: lim})
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, r := range rows {
			out = append(out, map[string]any{"id": r.Run.ID, "kind": r.Run.Kind, "state": r.Run.State, "task": r.TaskSqid,
				"title": r.TaskTitle, "project": r.ProjectSlug, "runner": r.RunnerName, "status": r.Run.StatusText,
				"error": r.Run.Error, "queued_at": r.Run.QueuedAt})
		}
		return out, nil

	case "task_get":
		if err := need(a.Sqid, "sqid"); err != nil {
			return nil, err
		}
		t, err := s.task(ctx, sc, a.Sqid)
		if err != nil {
			return nil, err
		}
		out := taskJSON(t)
		out["prompt"], out["created_by"], out["summary"] = t.Prompt, t.CreatedBy, t.CompletionSummary
		if t.ParentID.Valid {
			if p, err := s.DB.RQ.GetTask(ctx, t.ParentID.Int64); err == nil {
				out["parent"] = p.Sqid.String
			}
		}
		kids, _ := s.DB.RQ.ListChildren(ctx, sql.NullInt64{Int64: t.ID, Valid: true})
		var ks []map[string]any
		for _, k := range kids {
			ks = append(ks, taskJSON(k))
		}
		out["subtasks"] = ks
		pres, _ := s.DB.RQ.ListPrecursors(ctx, t.ID)
		var ps []map[string]any
		for _, p := range pres {
			full, _ := s.Stack.Task(ctx, s.DB.RQ, p.Sqid)
			sat := p.State == "done" || (p.State == "in_review" && sc.RunID != 0 && full.CompletedRunID.Int64 == sc.RunID)
			ps = append(ps, map[string]any{"sqid": p.Sqid, "title": p.Title, "state": p.State, "satisfied": sat})
		}
		out["precursors"] = ps
		out["repo"], out["effective_repo"] = "", ""
		if t.RepoID.Valid {
			if r, err := s.DB.RQ.GetRepo(ctx, t.RepoID.Int64); err == nil {
				out["repo"] = r.Owner + "/" + r.Name
			}
		}
		if id, err := reporesolve.Resolve(ctx, s.DB.RQ, t.ID); err == nil && id != 0 {
			if r, err := s.DB.RQ.GetRepo(ctx, id); err == nil {
				out["effective_repo"] = r.Owner + "/" + r.Name
			}
		}
		return out, nil

	case "repo_list":
		pid := sc.ProjectID
		if pid == 0 {
			if a.Project == "" {
				return nil, fmt.Errorf("%w: project is required", errs.Invalid)
			}
			p, err := s.DB.RQ.GetProjectBySlug(ctx, a.Project)
			if err != nil {
				return nil, fmt.Errorf("%w: project %q", errs.NotFound, a.Project)
			}
			pid = p.ID
		}
		proj, err := s.DB.RQ.GetProject(ctx, pid)
		if err != nil {
			return nil, err
		}
		rows, err := s.DB.RQ.ListRepos(ctx, pid)
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, r := range rows {
			out = append(out, map[string]any{"repo": r.Repo.Owner + "/" + r.Repo.Name, "default_branch": r.Repo.DefaultBranch,
				"note": r.Repo.Note, "default": proj.DefaultRepoID.Valid && proj.DefaultRepoID.Int64 == r.Repo.ID})
		}
		return out, nil

	case "task_tree":
		root := sc.RootID
		if a.Sqid != "" {
			t, err := s.task(ctx, sc, a.Sqid)
			if err != nil {
				return nil, err
			}
			root = t.ID
		}
		if root == 0 {
			return nil, fmt.Errorf("%w: sqid is required", errs.Invalid)
		}
		rows, err := s.DB.RQ.SubtreeOrdered(ctx, root)
		if err != nil {
			return nil, err
		}
		var out []map[string]any
		for _, r := range rows {
			out = append(out, map[string]any{"sqid": r.Sqid.String, "title": r.Title, "state": r.State, "depth": r.Depth})
		}
		return out, nil

	case "task_list":
		slug := a.Project
		if sc.ProjectID != 0 {
			p, err := s.DB.RQ.GetProject(ctx, sc.ProjectID)
			if err != nil {
				return nil, err
			}
			slug = p.Slug
		}
		rows, err := s.DB.RQ.ListTasks(ctx, dbgen.ListTasksParams{Project: slug, State: a.State})
		if err != nil {
			return nil, err
		}
		var out []map[string]any
		for _, r := range rows {
			if a.Text == "" || strings.Contains(strings.ToLower(r.Task.Title), strings.ToLower(a.Text)) {
				m := taskJSON(r.Task)
				m["project"] = r.ProjectSlug
				out = append(out, m)
			}
		}
		return out, nil

	case "task_next":
		run, err := s.DB.RQ.GetRun(ctx, sc.RunID)
		if err != nil {
			return nil, err
		}
		leaf, err := reporesolve.NextReadyLeaf(ctx, s.DB.RQ, sc.RootID, sc.RunID, run.RepoID.Int64) // only this run's repo
		if errors.Is(err, sql.ErrNoRows) {
			open, _ := reporesolve.CountOpenLeaves(ctx, s.DB.RQ, sc.RootID, run.RepoID.Int64)
			return map[string]any{"done": open == 0, "blocked": open > 0}, nil
		} else if err != nil {
			return nil, err
		}
		return map[string]any{"sqid": leaf.Sqid.String, "title": leaf.Title}, nil

	case "task_create":
		if err := need(a.Title, "title"); err != nil {
			return nil, err
		}
		project := a.Project
		if a.Parent != "" {
			p, err := s.writable(ctx, sc, a.Parent)
			if err != nil {
				return nil, err
			}
			pr, err := s.DB.RQ.GetProject(ctx, p.ProjectID)
			if err != nil {
				return nil, err
			}
			project = pr.Slug
		} else if sc.RootID != 0 {
			return nil, fmt.Errorf("%w: parent is required (a task in your assignment)", errs.Invalid)
		} else if project == "" {
			return nil, fmt.Errorf("%w: give parent, or project for a top-level task", errs.Invalid)
		}
		t, err := s.Stack.Create(ctx, stack.CreateParams{Project: project, Parent: a.Parent, Title: a.Title, Prompt: a.Prompt,
			Pos: a.Position.pos(), Needs: a.Needs, Need: a.Need, Type: a.Type, TargetDate: deref(a.TargetDate), Repo: deref(a.Repo), Actor: sc.Actor})
		if err != nil {
			return nil, err
		}
		return taskJSON(t), nil

	case "task_update":
		if _, err := s.writable(ctx, sc, a.Sqid); err != nil {
			return nil, err
		}
		var title, prompt *string
		if a.Title != "" {
			title = &a.Title
		}
		if raw != nil && strings.Contains(string(raw), `"prompt"`) {
			prompt = &a.Prompt
		}
		var t dbgen.Task
		var err error
		if a.Type != "" {
			if t, err = s.Stack.SetType(ctx, a.Sqid, a.Type, sc.Actor); err != nil {
				return nil, err
			}
		}
		if a.TargetDate != nil {
			if t, err = s.Stack.SetTargetDate(ctx, a.Sqid, *a.TargetDate, sc.Actor); err != nil {
				return nil, err
			}
		}
		if a.Repo != nil {
			if t, err = s.Stack.SetRepo(ctx, a.Sqid, *a.Repo, sc.Actor); err != nil {
				return nil, err
			}
		}
		if len(a.Need) > 0 {
			if err := s.Stack.SetNeed(ctx, a.Sqid, a.Need, sc.Actor); err != nil {
				return nil, err
			}
			if t.ID == 0 {
				if t, err = s.Stack.Task(ctx, s.DB.RQ, a.Sqid); err != nil {
					return nil, err
				}
			}
		}
		if title != nil || prompt != nil || (a.Type == "" && a.TargetDate == nil && a.Repo == nil && len(a.Need) == 0) {
			if t, err = s.Stack.Edit(ctx, a.Sqid, title, prompt, sc.Actor); err != nil {
				return nil, err
			}
		}
		return taskJSON(t), nil

	case "task_move":
		if _, err := s.writable(ctx, sc, a.Sqid); err != nil {
			return nil, err
		}
		var parent *string
		if a.Parent != "" {
			if _, err := s.writable(ctx, sc, a.Parent); err != nil {
				return nil, err
			}
			parent = &a.Parent
		}
		t, err := s.Stack.Move(ctx, a.Sqid, parent, a.Position.pos(), sc.Actor)
		if err != nil {
			return nil, err
		}
		return taskJSON(t), nil

	case "task_needs_add", "task_needs_remove":
		if _, err := s.writable(ctx, sc, a.Sqid); err != nil {
			return nil, err
		}
		if _, err := s.task(ctx, sc, a.Precursor); err != nil {
			return nil, err
		}
		var err error
		if tool == "task_needs_add" {
			err = s.Stack.NeedsAdd(ctx, a.Sqid, a.Precursor, sc.Actor)
		} else {
			err = s.Stack.NeedsRm(ctx, a.Sqid, a.Precursor, sc.Actor)
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil

	case "task_complete":
		if _, err := s.writable(ctx, sc, a.Sqid); err != nil {
			return nil, err
		}
		if sc.RunID == 0 {
			return map[string]any{"ok": true}, s.Stack.Complete(ctx, a.Sqid, a.Summary, sc.Actor)
		}
		if err := s.Stack.AgentComplete(ctx, sc.RunID, a.Sqid, a.Summary); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "state": "in_review"}, nil

	case "task_reopen":
		if _, err := s.writable(ctx, sc, a.Sqid); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, s.Stack.Reopen(ctx, a.Sqid, sc.Actor)

	case "task_cancel", "task_delete":
		t, err := s.writable(ctx, sc, a.Sqid)
		if err != nil {
			return nil, err
		}
		if sc.RunID != 0 && t.CreatedBy != string(sc.Actor) {
			payload, _ := json.Marshal(approvalPayload{Op: tool, Sqid: a.Sqid, Reason: a.Reason, RunID: sc.RunID})
			verb := strings.TrimPrefix(tool, "task_")
			q, err := s.NewQuestion(ctx, dbgen.CreateQuestionParams{
				TaskID: t.ID, RunID: sql.NullInt64{Int64: sc.RunID, Valid: true}, Kind: "approval",
				Prompt:   fmt.Sprintf("The agent on run %d wants to %s %s %q.", sc.RunID, verb, a.Sqid, t.Title),
				DetailMd: a.Reason, PayloadJson: string(payload),
			}, []proto.QuestionOption{{Label: "Approve", Value: "approve"}, {Label: "Deny", Value: "deny"}})
			if err != nil {
				return nil, err
			}
			// The run keeps going while the owner decides.
			s.DB.Q.SetRunState(ctx, dbgen.SetRunStateParams{State: "running", ID: sc.RunID, FromState: "needs_input"})
			return map[string]any{"pending_approval": true, "question_id": q.ID,
				"note": "The owner must approve this; carry on with other work."}, nil
		}
		if tool == "task_cancel" {
			return map[string]any{"ok": true}, s.Stack.Cancel(ctx, a.Sqid, a.Reason, sc.Actor)
		}
		return map[string]any{"ok": true}, s.Stack.Delete(ctx, a.Sqid, sc.Actor)

	case "ask_question", SystemQuestionTool:
		if err := need(a.Prompt, "prompt"); err != nil {
			return nil, err
		}
		if sc.RunID == 0 {
			return nil, fmt.Errorf("%w: ask_question needs a run", errs.Invalid)
		}
		run, err := s.DB.RQ.GetRun(ctx, sc.RunID)
		if err != nil {
			return nil, err
		}
		taskID := run.TaskID
		if run.CurrentTaskID.Valid {
			taskID = run.CurrentTaskID.Int64
		}
		kind := "agent"
		if tool == SystemQuestionTool {
			kind = "system"
		}
		if len(a.Options) == 0 && !a.AllowFreeText {
			a.AllowFreeText = true
		}
		ft := int64(0)
		if a.AllowFreeText {
			ft = 1
		}
		if kind == "system" {
			// One blocker, one card: fold the agent's own still-open questions for this
			// run into the system question (quoted in its body) and cancel them.
			folded, err := s.foldAgentQuestions(ctx, sc.RunID)
			if err != nil {
				return nil, err
			}
			if folded != "" {
				a.DetailMD = strings.TrimSpace(a.DetailMD + "\n\n" + folded)
			}
		}
		q, err := s.NewQuestion(ctx, dbgen.CreateQuestionParams{TaskID: taskID, RunID: sql.NullInt64{Int64: sc.RunID, Valid: true},
			Kind: kind, Prompt: a.Prompt, DetailMd: a.DetailMD, AllowFreeText: ft}, a.Options)
		if err != nil {
			return nil, err
		}
		if run.Kind == "resolve" && run.PrID.Valid {
			// A Smart Merge question blocks the PR: the Inbox nests it under the review card.
			if err := s.DB.Q.SetQuestionPR(ctx, dbgen.SetQuestionPRParams{PrID: run.PrID, ID: q.ID}); err != nil {
				return nil, err
			}
		}
		return map[string]any{"question_id": q.ID}, nil

	case "report_status":
		status := a.Summary
		if a.Percent != nil {
			status = fmt.Sprintf("%s (%d%%)", status, *a.Percent)
		}
		if len(status) > 300 {
			status = status[:300]
		}
		defer s.Bus.Run(sc.RunID)
		return map[string]any{"ok": true}, s.DB.Q.SetRunStatus(ctx, dbgen.SetRunStatusParams{StatusText: status, ID: sc.RunID})

	case "submit_review", "propose_split", "submit_deliverable", "submit_citation_audit", "issue_search", "issue_get", "issue_comments",
		"issue_comment", "issue_create", "issue_edit", "issue_close", "issue_link_task",
		"project_get", "project_create", "project_edit", "project_archive", "project_restore", "project_delete":
		h := s.ToolHandlers[tool]
		if h == nil {
			return nil, fmt.Errorf("%s is not available on this controller", tool)
		}
		return h(ctx, sc, raw)
	}
	return nil, fmt.Errorf("%w: unknown tool %s", errs.Invalid, tool)
}

// ToolHandler implements a tool owned by another service (submit_review, propose_split).
type ToolHandler func(ctx context.Context, sc Scope, args json.RawMessage) (any, error)

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// foldAgentQuestions cancels the run's still-open agent questions and returns them quoted
// as markdown, for the system question that supersedes them ("" if there were none).
func (s *Service) foldAgentQuestions(ctx context.Context, runID int64) (string, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT id, prompt, detail_md FROM question WHERE run_id = ? AND kind = 'agent' AND state = 'open' ORDER BY id`, runID)
	if err != nil {
		return "", err
	}
	type oq struct {
		id             int64
		prompt, detail string
	}
	var open []oq
	for rows.Next() {
		var o oq
		if err := rows.Scan(&o.id, &o.prompt, &o.detail); err != nil {
			rows.Close()
			return "", err
		}
		open = append(open, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(open) == 0 {
		return "", err
	}
	var b strings.Builder
	b.WriteString("**The agent's own open questions (cancelled; answer them in your reply):**\n")
	for _, o := range open {
		n, err := s.DB.W.ExecContext(ctx, `UPDATE question SET state = 'cancelled' WHERE id = ? AND state = 'open'`, o.id)
		if err != nil {
			return "", err
		}
		if c, _ := n.RowsAffected(); c == 0 {
			continue // answered meanwhile
		}
		b.WriteString("\n> " + strings.ReplaceAll(strings.TrimSpace(o.prompt), "\n", "\n> ") + "\n")
		if d := strings.TrimSpace(o.detail); d != "" {
			b.WriteString(">\n> " + strings.ReplaceAll(d, "\n", "\n> ") + "\n")
		}
		s.Bus.Question(o.id)
	}
	return b.String(), nil
}

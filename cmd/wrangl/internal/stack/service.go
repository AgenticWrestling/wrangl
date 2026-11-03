package stack

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nreid/devhost/cmd/wrangl/internal/db"
	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbgen"
	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbtypes"
	"github.com/nreid/devhost/cmd/wrangl/internal/errs"
	"github.com/nreid/devhost/cmd/wrangl/internal/events"
)

// Actor identifies who changed the work stack: "user" or "agent:<run id>".
type Actor string

// User is the owner acting through the CLI, TUI or PWA.
const User Actor = "user"

// AgentActor is the agent working run runID.
func AgentActor(runID int64) Actor { return Actor("agent:" + strconv.FormatInt(runID, 10)) }

// Service is the only writer of task rows (spec §4). Every write records a task_event.
type Service struct {
	DB    *db.DB
	Sqids *Sqids
	Bus   *events.Bus // change notices (nil = none)
}

// Position says where among its siblings a task goes. Zero value = last.
type Position struct {
	First  bool
	After  string // sibling sqid
	Before string // sibling sqid
}

func invalid(f string, a ...any) error {
	return fmt.Errorf("%w: "+f, append([]any{errs.Invalid}, a...)...)
}
func conflict(f string, a ...any) error {
	return fmt.Errorf("%w: "+f, append([]any{errs.Conflict}, a...)...)
}

func nstr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// completedAt is set for terminal-ish states and cleared otherwise.
func completedAt(state string) sql.NullString {
	if state == "in_review" || state == "done" {
		return nstr(time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	return sql.NullString{}
}
func nint(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: v != 0} }

// Task loads a task by sqid.
func (s *Service) Task(ctx context.Context, q *dbgen.Queries, sqid string) (dbgen.Task, error) {
	t, err := q.GetTaskBySqid(ctx, nstr(sqid))
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("%w: task %q", errs.NotFound, sqid)
	}
	return t, err
}

func (s *Service) project(ctx context.Context, q *dbgen.Queries, slug string) (dbgen.Project, error) {
	p, err := q.GetProjectBySlug(ctx, slug)
	if errors.Is(err, sql.ErrNoRows) {
		return p, fmt.Errorf("%w: project %q", errs.NotFound, slug)
	}
	return p, err
}

func hasChildren(ctx context.Context, q *dbgen.Queries, id int64) (bool, error) {
	c, err := q.ChildStateCounts(ctx, nint(id))
	return c.Total > 0, err
}

// Changed publishes a task change notice (called by writes; also usable by other services).
func (s *Service) Changed(sqid string) { s.Bus.Publish("task", sqid) }

// event records an audit entry and publishes a change notice. Subscribers may refetch a
// moment before the surrounding transaction commits, so they debounce (see events).
func (s *Service) event(ctx context.Context, q *dbgen.Queries, sqid string, actor Actor, op string, diff map[string]any) error {
	defer s.Bus.Publish("task", sqid)
	b, _ := json.Marshal(diff)
	var runID sql.NullInt64
	if strings.HasPrefix(string(actor), "agent:") {
		if id, err := strconv.ParseInt(strings.TrimPrefix(string(actor), "agent:"), 10, 64); err == nil {
			runID = nint(id)
		}
	}
	return q.InsertTaskEvent(ctx, dbgen.InsertTaskEventParams{TaskSqid: sqid, RunID: runID, Actor: string(actor), Op: op, DiffJson: string(b)})
}

// rankFor computes a rank for a task placed at pos among the children of parentID (0 =
// project roots), ignoring the task being moved (excludeID).
func (s *Service) rankFor(ctx context.Context, q *dbgen.Queries, projectID, parentID, excludeID int64, pos Position) (string, error) {
	var sibs []dbgen.Task
	var err error
	if parentID == 0 {
		sibs, err = q.ListRootTasks(ctx, projectID)
	} else {
		sibs, err = q.ListChildren(ctx, nint(parentID))
	}
	if err != nil {
		return "", err
	}
	ranks := make([]string, 0, len(sibs))
	sqids := make([]string, 0, len(sibs))
	for _, t := range sibs {
		if t.ID != excludeID {
			ranks = append(ranks, t.Rank)
			sqids = append(sqids, t.Sqid.String)
		}
	}
	idx := len(ranks) // insert index
	switch {
	case pos.First:
		idx = 0
	case pos.After != "" || pos.Before != "":
		target := pos.After + pos.Before
		found := -1
		for i, q := range sqids {
			if q == target {
				found = i
			}
		}
		if found < 0 {
			return "", invalid("%s is not a sibling at the target position", target)
		}
		idx = found
		if pos.After != "" {
			idx = found + 1
		}
	}
	lo, hi := "", ""
	if idx > 0 {
		lo = ranks[idx-1]
	}
	if idx < len(ranks) {
		hi = ranks[idx]
	}
	return RankBetween(lo, hi)
}

// newTask inserts a task, assigns its sqid and records the create event.
func (s *Service) newTask(ctx context.Context, q *dbgen.Queries, p dbgen.CreateTaskParams, actor Actor) (dbgen.Task, error) {
	p.CreatedBy = string(actor)
	if p.TaskType == "" {
		p.TaskType = dbtypes.TaskCode
	}
	id, err := q.CreateTask(ctx, p)
	if err != nil {
		return dbgen.Task{}, err
	}
	sq, err := s.Sqids.Encode(id)
	if err != nil {
		return dbgen.Task{}, err
	}
	if err := q.SetTaskSqid(ctx, dbgen.SetTaskSqidParams{Sqid: nstr(sq), ID: id}); err != nil {
		return dbgen.Task{}, err
	}
	if err := s.event(ctx, q, sq, actor, "create", map[string]any{"title": p.Title}); err != nil {
		return dbgen.Task{}, err
	}
	return q.GetTask(ctx, id)
}

// recompute derives container states from their children, from node's parent upwards.
func recompute(ctx context.Context, q *dbgen.Queries, parentID sql.NullInt64) error {
	for parentID.Valid {
		c, err := q.ChildStateCounts(ctx, parentID)
		if err != nil {
			return err
		}
		state := ""
		switch {
		case c.Total == 0:
		case c.InProgress > 0:
			state = "in_progress"
		case c.Open > 0:
			state = "open"
		case c.InReview > 0:
			state = "in_review"
		case c.Done > 0:
			state = "done"
		default:
			state = "cancelled"
		}
		if state != "" {
			if err := q.SetContainerState(ctx, dbgen.SetContainerStateParams{State: state, ID: parentID.Int64}); err != nil {
				return err
			}
		}
		p, err := q.GetTask(ctx, parentID.Int64)
		if err != nil {
			return err
		}
		parentID = p.ParentID
	}
	return nil
}

// --- create / edit ------------------------------------------------------------------

// CreateParams describe a new task.
type CreateParams struct {
	Project  string // slug
	Parent   string // sqid; "" = top level
	Title    string
	Prompt   string
	Pos      Position
	Repo     string // owner/name, linked to the project; "" = inherit
	Template string // template name; "" = inherit
	Issue    string // owner/name#number; optional
	Needs    []string
	Type     string // dbtypes.TaskType name; "" = inherit the parent's, else code
	// TargetDate is YYYY-MM-DD; "" = none.
	TargetDate string
	Actor      Actor
}

// parseTargetDate checks a YYYY-MM-DD date; "" is valid (no date).
func parseTargetDate(s string) (sql.NullString, error) {
	if s == "" {
		return sql.NullString{}, nil
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return sql.NullString{}, invalid("target date %q: want YYYY-MM-DD", s)
	}
	return sql.NullString{String: s, Valid: true}, nil
}

// Create adds a task.
func (s *Service) Create(ctx context.Context, c CreateParams) (dbgen.Task, error) {
	if strings.TrimSpace(c.Title) == "" {
		return dbgen.Task{}, invalid("title is required")
	}
	if c.Actor == "" {
		c.Actor = User
	}
	td, err := parseTargetDate(c.TargetDate)
	if err != nil {
		return dbgen.Task{}, err
	}
	var out dbgen.Task
	err = s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		p, err := s.project(ctx, q, c.Project)
		if err != nil {
			return err
		}
		var parentID int64
		if c.Parent != "" {
			parent, err := s.Task(ctx, q, c.Parent)
			if err != nil {
				return err
			}
			if parent.ProjectID != p.ID {
				return invalid("parent %s is in another project", c.Parent)
			}
			if err := canGainChildren(ctx, q, parent); err != nil {
				return err
			}
			parentID = parent.ID
		}
		rank, err := s.rankFor(ctx, q, p.ID, parentID, 0, c.Pos)
		if err != nil {
			return err
		}
		tt, err := dbtypes.ParseTaskType(c.Type)
		if err != nil {
			return invalid("%v", err)
		}
		if c.Type == "" && parentID != 0 {
			if pt, err := q.GetTask(ctx, parentID); err == nil {
				if !pt.TaskType.Planning() {
					tt = pt.TaskType // subtasks inherit the parent's type unless told otherwise
				} // (a release, milestone or feature's children are ordinary tasks)
			}
		}
		params := dbgen.CreateTaskParams{ProjectID: p.ID, ParentID: nint(parentID), Rank: rank, Title: c.Title, Prompt: c.Prompt, TaskType: tt}
		if c.Repo != "" {
			r, err := projectRepo(ctx, q, p.ID, c.Repo)
			if err != nil {
				return err
			}
			params.RepoID = nint(r.ID)
		}
		if c.Template != "" {
			t, err := q.GetTemplateByName(ctx, c.Template)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: template %q", errs.NotFound, c.Template)
			} else if err != nil {
				return err
			}
			params.TemplateID = nint(t.ID)
		}
		if out, err = s.newTask(ctx, q, params, c.Actor); err != nil {
			return err
		}
		if td.Valid {
			if err := q.SetTaskTargetDate(ctx, dbgen.SetTaskTargetDateParams{TargetDate: td, ID: out.ID}); err != nil {
				return err
			}
			out.TargetDate = td
		}
		if c.Issue != "" {
			iss, err := issueByRef(ctx, q, p.ID, c.Issue)
			if err != nil {
				return err
			}
			// Inside the transaction: never use s.DB.W here (one writer connection, held by us).
			if err := q.SetTaskIssue(ctx, dbgen.SetTaskIssueParams{IssueID: nint(iss.ID), ID: out.ID}); err != nil {
				return err
			}
			out.IssueID = nint(iss.ID)
		}
		for _, pre := range c.Needs {
			if err := s.addNeed(ctx, q, out, pre, c.Actor); err != nil {
				return err
			}
		}
		return recompute(ctx, q, out.ParentID)
	})
	return out, err
}

// canGainChildren refuses to turn a leaf the agent is working on (or has finished) into a
// container.
func canGainChildren(ctx context.Context, q *dbgen.Queries, parent dbgen.Task) error {
	kids, err := hasChildren(ctx, q, parent.ID)
	if err != nil {
		return err
	}
	if !kids && (parent.State == "in_progress" || parent.State == "in_review" || parent.State == "done") {
		return conflict("%s is a %s leaf; it cannot gain subtasks", parent.Sqid.String, parent.State)
	}
	return nil
}

func projectRepo(ctx context.Context, q *dbgen.Queries, projectID int64, ref string) (dbgen.Repo, error) {
	owner, name, ok := strings.Cut(ref, "/")
	if !ok || owner == "" || name == "" {
		return dbgen.Repo{}, invalid("repo %q: want owner/name", ref)
	}
	r, err := q.GetProjectRepo(ctx, dbgen.GetProjectRepoParams{ProjectID: projectID, Owner: owner, Name: name})
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("%w: repo %s is not linked to this project (wrangl repo link)", errs.NotFound, ref)
	}
	return r, err
}

func issueByRef(ctx context.Context, q *dbgen.Queries, projectID int64, ref string) (dbgen.Issue, error) {
	repo, num, ok := strings.Cut(ref, "#")
	owner, name, ok2 := strings.Cut(repo, "/")
	n, err := strconv.ParseInt(num, 10, 64)
	if !ok || !ok2 || err != nil {
		return dbgen.Issue{}, invalid("issue %q: want owner/name#number", ref)
	}
	iss, err := q.GetIssueByNumber(ctx, dbgen.GetIssueByNumberParams{ProjectID: projectID, Owner: owner, Name: name, Number: n})
	if errors.Is(err, sql.ErrNoRows) {
		return iss, fmt.Errorf("%w: issue %s (run wrangl issue sync)", errs.NotFound, ref)
	}
	return iss, err
}

// Edit changes a task's title and/or prompt (nil = unchanged).
func (s *Service) Edit(ctx context.Context, sqid string, title, prompt *string, actor Actor) (dbgen.Task, error) {
	var out dbgen.Task
	err := s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		t, err := s.Task(ctx, q, sqid)
		if err != nil {
			return err
		}
		diff := map[string]any{}
		nt, np := t.Title, t.Prompt
		if title != nil {
			if strings.TrimSpace(*title) == "" {
				return invalid("title cannot be empty")
			}
			nt, diff["title"] = *title, *title
		}
		if prompt != nil {
			np, diff["prompt"] = *prompt, "changed"
		}
		if out, err = q.UpdateTaskText(ctx, dbgen.UpdateTaskTextParams{Title: nt, Prompt: np, ID: t.ID}); err != nil {
			return err
		}
		return s.event(ctx, q, sqid, actor, "update", diff)
	})
	return out, err
}

// SetType changes a task's type.
func (s *Service) SetType(ctx context.Context, sqid, typ string, actor Actor) (dbgen.Task, error) {
	tt, err := dbtypes.ParseTaskType(typ)
	if err != nil {
		return dbgen.Task{}, invalid("%v", err)
	}
	var out dbgen.Task
	err = s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		t, err := s.Task(ctx, q, sqid)
		if err != nil {
			return err
		}
		if t.TaskType == tt {
			out = t
			return nil
		}
		if err := q.SetTaskType(ctx, dbgen.SetTaskTypeParams{TaskType: tt, ID: t.ID}); err != nil {
			return err
		}
		if out, err = q.GetTask(ctx, t.ID); err != nil {
			return err
		}
		return s.event(ctx, q, sqid, actor, "update", map[string]any{"type": string(tt)})
	})
	return out, err
}

// SetTargetDate sets a task's target date (YYYY-MM-DD); "" clears it.
func (s *Service) SetTargetDate(ctx context.Context, sqid, date string, actor Actor) (dbgen.Task, error) {
	td, err := parseTargetDate(date)
	if err != nil {
		return dbgen.Task{}, err
	}
	var out dbgen.Task
	err = s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		t, err := s.Task(ctx, q, sqid)
		if err != nil {
			return err
		}
		if t.TargetDate == td {
			out = t
			return nil
		}
		if err := q.SetTaskTargetDate(ctx, dbgen.SetTaskTargetDateParams{TargetDate: td, ID: t.ID}); err != nil {
			return err
		}
		if out, err = q.GetTask(ctx, t.ID); err != nil {
			return err
		}
		return s.event(ctx, q, sqid, actor, "update", map[string]any{"target_date": date})
	})
	return out, err
}

// --- move / precursors -----------------------------------------------------------------

// Move reparents and/or reranks a task. newParent: nil = keep parent, "" = top level.
func (s *Service) Move(ctx context.Context, sqid string, newParent *string, pos Position, actor Actor) (dbgen.Task, error) {
	var out dbgen.Task
	err := s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		t, err := s.Task(ctx, q, sqid)
		if err != nil {
			return err
		}
		parentID := t.ParentID.Int64
		op := "rerank"
		if newParent != nil {
			op = "move"
			parentID = 0
			if *newParent != "" {
				np, err := s.Task(ctx, q, *newParent)
				if err != nil {
					return err
				}
				if np.ProjectID != t.ProjectID {
					return invalid("cannot move a task to another project")
				}
				hit, err := q.IsAncestorOrSelf(ctx, dbgen.IsAncestorOrSelfParams{NodeID: np.ID, AncestorID: t.ID})
				if err != nil {
					return err
				}
				if hit == 1 {
					return invalid("cannot move %s under itself or its own subtask", sqid)
				}
				if err := canGainChildren(ctx, q, np); err != nil {
					return err
				}
				parentID = np.ID
			}
			if parentID != t.ParentID.Int64 && (t.State == "in_progress" || t.State == "in_review") {
				return conflict("%s is %s; it cannot change parent", sqid, t.State)
			}
		}
		rank, err := s.rankFor(ctx, q, t.ProjectID, parentID, t.ID, pos)
		if err != nil {
			return err
		}
		if err := q.MoveTask(ctx, dbgen.MoveTaskParams{ParentID: nint(parentID), Rank: rank, ID: t.ID}); err != nil {
			return err
		}
		if err := s.event(ctx, q, sqid, actor, op, map[string]any{"parent_id": parentID, "rank": rank}); err != nil {
			return err
		}
		if err := recompute(ctx, q, t.ParentID); err != nil {
			return err
		}
		if err := recompute(ctx, q, nint(parentID)); err != nil {
			return err
		}
		out, err = q.GetTask(ctx, t.ID)
		return err
	})
	return out, err
}

func (s *Service) addNeed(ctx context.Context, q *dbgen.Queries, t dbgen.Task, preSqid string, actor Actor) error {
	pre, err := s.Task(ctx, q, preSqid)
	if err != nil {
		return err
	}
	if pre.ID == t.ID {
		return invalid("a task cannot need itself")
	}
	if pre.ProjectID != t.ProjectID {
		return invalid("precursor %s is in another project", preSqid)
	}
	for _, pair := range [][2]int64{{t.ID, pre.ID}, {pre.ID, t.ID}} {
		hit, err := q.IsAncestorOrSelf(ctx, dbgen.IsAncestorOrSelfParams{NodeID: pair[0], AncestorID: pair[1]})
		if err != nil {
			return err
		}
		if hit == 1 {
			return invalid("%s and %s are ancestor and descendant; that dependency is implicit", t.Sqid.String, preSqid)
		}
	}
	// Adding t -> pre cycles if pre already (transitively) needs t.
	hit, err := q.PrecursorReaches(ctx, dbgen.PrecursorReachesParams{FromID: pre.ID, ToID: t.ID})
	if err != nil {
		return err
	}
	if hit == 1 {
		return invalid("%s already depends on %s; that would be a cycle", preSqid, t.Sqid.String)
	}
	if err := q.AddPrecursor(ctx, dbgen.AddPrecursorParams{TaskID: t.ID, DependsOnID: pre.ID, CreatedBy: string(actor)}); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return conflict("%s already needs %s", t.Sqid.String, preSqid)
		}
		return err
	}
	return s.event(ctx, q, t.Sqid.String, actor, "add_precursor", map[string]any{"precursor": preSqid})
}

// NeedsAdd makes sqid depend on pre.
func (s *Service) NeedsAdd(ctx context.Context, sqid, pre string, actor Actor) error {
	return s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		t, err := s.Task(ctx, q, sqid)
		if err != nil {
			return err
		}
		return s.addNeed(ctx, q, t, pre, actor)
	})
}

// NeedsRm removes a dependency.
func (s *Service) NeedsRm(ctx context.Context, sqid, pre string, actor Actor) error {
	return s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		t, err := s.Task(ctx, q, sqid)
		if err != nil {
			return err
		}
		p, err := s.Task(ctx, q, pre)
		if err != nil {
			return err
		}
		n, err := q.RemovePrecursor(ctx, dbgen.RemovePrecursorParams{TaskID: t.ID, DependsOnID: p.ID})
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: %s does not need %s", errs.NotFound, sqid, pre)
		}
		return s.event(ctx, q, sqid, actor, "remove_precursor", map[string]any{"precursor": pre})
	})
}

// --- state changes ------------------------------------------------------------------

func (s *Service) setLeafState(ctx context.Context, sqid, state, summary string, actor Actor, op string) error {
	return s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		t, err := s.Task(ctx, q, sqid)
		if err != nil {
			return err
		}
		if kids, err := hasChildren(ctx, q, t.ID); err != nil {
			return err
		} else if kids {
			return invalid("%s has subtasks; its state follows them", sqid)
		}
		if err := q.SetTaskState(ctx, dbgen.SetTaskStateParams{State: state, Summary: summary, CompletedAt: completedAt(state), ID: t.ID}); err != nil {
			return err
		}
		if err := s.event(ctx, q, sqid, actor, op, map[string]any{"state": state}); err != nil {
			return err
		}
		return recompute(ctx, q, t.ParentID)
	})
}

// Complete marks a leaf done by hand (the owner says the work is finished and merged).
func (s *Service) Complete(ctx context.Context, sqid, summary string, actor Actor) error {
	return s.setLeafState(ctx, sqid, "done", summary, actor, "complete")
}

// Reopen returns a leaf to open.
func (s *Service) Reopen(ctx context.Context, sqid string, actor Actor) error {
	return s.setLeafState(ctx, sqid, "open", "", actor, "reopen")
}

// Cancel cancels a task and every unfinished task beneath it.
func (s *Service) Cancel(ctx context.Context, sqid, reason string, actor Actor) error {
	return s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		t, err := s.Task(ctx, q, sqid)
		if err != nil {
			return err
		}
		rows, err := q.SubtreeOrdered(ctx, t.ID)
		if err != nil {
			return err
		}
		// Leaves first (deepest), so containers are recomputed from final child states.
		for i := len(rows) - 1; i >= 0; i-- {
			r := rows[i]
			if kids, _ := hasChildren(ctx, q, r.ID); kids || r.State == "done" || r.State == "cancelled" {
				continue
			}
			if err := q.SetTaskState(ctx, dbgen.SetTaskStateParams{State: "cancelled", Summary: reason, CompletedAt: completedAt("cancelled"), ID: r.ID}); err != nil {
				return err
			}
			if err := s.event(ctx, q, r.Sqid.String, actor, "cancel", map[string]any{"reason": reason}); err != nil {
				return err
			}
			if err := recompute(ctx, q, r.ParentID); err != nil {
				return err
			}
		}
		return nil
	})
}

// Delete removes a task and its subtree; refused while a run works on any of it.
func (s *Service) Delete(ctx context.Context, sqid string, actor Actor) error {
	return s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		t, err := s.Task(ctx, q, sqid)
		if err != nil {
			return err
		}
		locked, err := q.SubtreeLocked(ctx, t.ID)
		if err != nil {
			return err
		}
		if locked == 1 {
			return conflict("%s is part of a subtree a run is working on", sqid)
		}
		if err := q.DeleteTask(ctx, t.ID); err != nil {
			return err
		}
		if err := s.event(ctx, q, sqid, actor, "delete", map[string]any{"title": t.Title}); err != nil {
			return err
		}
		return recompute(ctx, q, t.ParentID)
	})
}

// --- split -----------------------------------------------------------------------

// SplitNode is one subtask in a split (children nest).
type SplitNode struct {
	Title    string      `json:"title"`
	Prompt   string      `json:"prompt,omitempty"`
	Type     string      `json:"type,omitempty"` // "" = the split task's type
	Children []SplitNode `json:"children,omitempty"`
}

// ParseOutline turns "one subtask per line, indent (2 spaces / tab) or markdown bullets
// to nest" into a tree. Blank lines are ignored.
func ParseOutline(text string) ([]SplitNode, error) {
	type frame struct {
		indent int
		nodes  *[]SplitNode
	}
	var roots []SplitNode
	stack := []frame{{-1, &roots}}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.ReplaceAll(raw, "\t", "  ")
		trimmed := strings.TrimLeft(line, " ")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		indent := len(line) - len(trimmed)
		for _, b := range []string{"- [ ] ", "- ", "* ", "+ "} {
			if strings.HasPrefix(trimmed, b) {
				trimmed = trimmed[len(b):]
				break
			}
		}
		title := strings.TrimSpace(trimmed)
		typ := ""
		if strings.HasPrefix(title, "[") { // "[docs] Update the README" sets the type
			if i := strings.Index(title, "]"); i > 1 {
				if tt, err := dbtypes.ParseTaskType(strings.ToLower(title[1:i])); err == nil {
					typ, title = string(tt), strings.TrimSpace(title[i+1:])
				}
			}
		}
		for len(stack) > 1 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1]
		*parent.nodes = append(*parent.nodes, SplitNode{Title: title, Type: typ})
		added := &(*parent.nodes)[len(*parent.nodes)-1]
		stack = append(stack, frame{indent, &added.Children})
	}
	if len(roots) == 0 {
		return nil, invalid("the outline has no subtasks")
	}
	return roots, nil
}

// Split adds subtasks under sqid atomically. chain makes each top-level new subtask need
// the previous one; copyPrompt copies the parent's prompt into subtasks without one.
func (s *Service) Split(ctx context.Context, sqid string, nodes []SplitNode, chain, copyPrompt bool, actor Actor) ([]dbgen.Task, error) {
	if len(nodes) == 0 {
		return nil, invalid("no subtasks given")
	}
	var created []dbgen.Task
	err := s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		parent, err := s.Task(ctx, q, sqid)
		if err != nil {
			return err
		}
		if parent.State == "in_progress" || parent.State == "in_review" || parent.State == "done" {
			return conflict("%s is %s; only open tasks can be split", sqid, parent.State)
		}
		var add func(parentID int64, ns []SplitNode, top bool) error
		var prev dbgen.Task
		add = func(parentID int64, ns []SplitNode, top bool) error {
			for _, n := range ns {
				if strings.TrimSpace(n.Title) == "" {
					return invalid("every subtask needs a title")
				}
				rank, err := s.rankFor(ctx, q, parent.ProjectID, parentID, 0, Position{})
				if err != nil {
					return err
				}
				prompt := n.Prompt
				if prompt == "" && copyPrompt {
					prompt = parent.Prompt
				}
				tt := parent.TaskType
				if n.Type != "" {
					if tt, err = dbtypes.ParseTaskType(n.Type); err != nil {
						return invalid("%v", err)
					}
				}
				t, err := s.newTask(ctx, q, dbgen.CreateTaskParams{ProjectID: parent.ProjectID, ParentID: nint(parentID), Rank: rank, Title: n.Title, Prompt: prompt, TaskType: tt}, actor)
				if err != nil {
					return err
				}
				created = append(created, t)
				if top && chain && prev.ID != 0 {
					if err := s.addNeed(ctx, q, t, prev.Sqid.String, actor); err != nil {
						return err
					}
				}
				if top {
					prev = t
				}
				if err := add(t.ID, n.Children, false); err != nil {
					return err
				}
			}
			return nil
		}
		if err := add(parent.ID, nodes, true); err != nil {
			return err
		}
		return recompute(ctx, q, nint(parent.ID))
	})
	return created, err
}

// --- queue -------------------------------------------------------------------------

// Queue creates a work run for the subtree rooted at sqid.
func (s *Service) Queue(ctx context.Context, sqid string) (dbgen.Run, error) {
	var run dbgen.Run
	err := s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		t, err := s.Task(ctx, q, sqid)
		if err != nil {
			return err
		}
		locked, err := q.SubtreeLocked(ctx, t.ID)
		if err != nil {
			return err
		}
		if locked == 1 {
			return conflict("%s overlaps a subtree a run is already working on", sqid)
		}
		open, err := q.CountOpenLeaves(ctx, t.ID)
		if err != nil {
			return err
		}
		if open == 0 {
			return conflict("%s has no open work", sqid)
		}
		chain, err := q.AncestorChain(ctx, t.ID)
		if err != nil {
			return err
		}
		proj, err := q.GetProject(ctx, t.ProjectID)
		if err != nil {
			return err
		}
		var templateID, repoID int64
		for _, a := range chain { // node first, then ancestors
			if templateID == 0 && a.TemplateID.Valid {
				templateID = a.TemplateID.Int64
			}
			if repoID == 0 && a.RepoID.Valid {
				repoID = a.RepoID.Int64
			}
		}
		if templateID == 0 {
			templateID = proj.DefaultTemplateID.Int64
		}
		if templateID == 0 {
			return invalid("no runner template: set one on the task, an ancestor, or the project (--template)")
		}
		if repoID == 0 {
			rc, err := q.ListProjectRepoCount(ctx, proj.ID)
			if err != nil {
				return err
			}
			if rc.N != 1 {
				return invalid("no repo: set one on the task or an ancestor (project %s has %d linked repos)", proj.Slug, rc.N)
			}
			repoID = rc.OnlyID
		}
		repos, err := q.SubtreeRepoIDs(ctx, t.ID)
		if err != nil {
			return err
		}
		for _, r := range repos {
			if r != repoID {
				return invalid("subtree %s spans more than one repo; queue its single-repo subtrees separately", sqid)
			}
		}
		n, err := q.CountRunsForTask(ctx, t.ID)
		if err != nil {
			return err
		}
		b := make([]byte, 16)
		rand.Read(b)
		b[6] = (b[6] & 0x0f) | 0x40 // UUID v4
		b[8] = (b[8] & 0x3f) | 0x80
		h := hex.EncodeToString(b)
		uuid := h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
		run, err = q.QueueRun(ctx, dbgen.QueueRunParams{
			TaskID: t.ID, Kind: "work", Attempt: n + 1, TemplateID: nint(templateID), RepoID: nint(repoID),
			Branch: fmt.Sprintf("wrangl/%s-%d", sqid, n+1), SessionUuid: uuid,
		})
		return err
	})
	return run, err
}

// --- the agent's side (M4 subset; M5 adds the full MCP tool set) ---------------------

// Next is the reply to a runner asking for its next leaf.
type Next struct {
	Done    bool   `json:"done"`              // no open leaves remain
	Blocked bool   `json:"blocked,omitempty"` // open leaves remain, none ready
	Sqid    string `json:"sqid,omitempty"`
	Title   string `json:"title,omitempty"`
	Prompt  string `json:"prompt,omitempty"` // the full turn prompt
}

// NextForRun picks the run's next ready leaf, marks it in progress and composes the turn
// prompt.
func (s *Service) NextForRun(ctx context.Context, runID int64) (Next, error) {
	var out Next
	err := s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		run, err := q.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		if run.Kind != "work" { // review / revise / plan: one composed prompt, then done
			n, err := q.UseOverride(ctx, runID)
			if err != nil {
				return err
			}
			if n == 0 || run.PromptOverride == "" {
				out = Next{Done: true}
				return nil
			}
			root, err := q.GetTask(ctx, run.TaskID)
			if err != nil {
				return err
			}
			out = Next{Sqid: root.Sqid.String, Title: run.Kind + " " + root.Title, Prompt: run.PromptOverride}
			return nil
		}
		leaf, err := q.NextReadyLeaf(ctx, dbgen.NextReadyLeafParams{RootID: run.TaskID, RunID: nint(runID)})
		if errors.Is(err, sql.ErrNoRows) {
			open, err := q.CountOpenLeaves(ctx, run.TaskID)
			if err != nil {
				return err
			}
			out = Next{Done: open == 0, Blocked: open > 0}
			return q.SetRunCurrentTask(ctx, dbgen.SetRunCurrentTaskParams{ID: runID})
		} else if err != nil {
			return err
		}
		t, err := q.GetTask(ctx, leaf.ID)
		if err != nil {
			return err
		}
		if t.State == "open" {
			if err := q.SetTaskState(ctx, dbgen.SetTaskStateParams{State: "in_progress", ID: t.ID}); err != nil {
				return err
			}
			if err := recompute(ctx, q, t.ParentID); err != nil {
				return err
			}
		}
		if err := q.SetRunCurrentTask(ctx, dbgen.SetRunCurrentTaskParams{TaskID: nint(t.ID), ID: runID}); err != nil {
			return err
		}
		prompt, err := composePrompt(ctx, q, t, runID)
		if err != nil {
			return err
		}
		out = Next{Sqid: t.Sqid.String, Title: t.Title, Prompt: prompt}
		return nil
	})
	return out, err
}

func composePrompt(ctx context.Context, q *dbgen.Queries, t dbgen.Task, runID int64) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "You are working on task %s: %s\n", t.Sqid.String, t.Title)
	if g := t.TaskType.Guidance(); g != "" {
		fmt.Fprintf(&b, "\n%s\n", g)
	}
	if strings.TrimSpace(t.Prompt) != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimSpace(t.Prompt))
	}
	chain, err := q.AncestorChain(ctx, t.ID)
	if err != nil {
		return "", err
	}
	if len(chain) > 1 {
		b.WriteString("\nContext from the parent tasks (outermost first):\n")
		for i := len(chain) - 1; i >= 1; i-- {
			a := chain[i]
			fmt.Fprintf(&b, "- %s %s", a.Sqid.String, a.Title)
			if p := strings.TrimSpace(a.Prompt); p != "" {
				fmt.Fprintf(&b, ": %s", p)
			}
			b.WriteString("\n")
		}
	}
	done, err := q.CompletedInRun(ctx, nint(runID))
	if err != nil {
		return "", err
	}
	if len(done) > 0 {
		b.WriteString("\nAlready completed earlier in this session:\n")
		for _, d := range done {
			fmt.Fprintf(&b, "- %s %s", d.Sqid, d.Title)
			if d.CompletionSummary != "" {
				fmt.Fprintf(&b, ": %s", d.CompletionSummary)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\nWork only on this task. Commit your changes with a clear message. " +
		"When the task is finished, end your turn.\n")
	return b.String(), nil
}

// AgentComplete records that the agent finished a leaf in its run (-> in_review).
func (s *Service) AgentComplete(ctx context.Context, runID int64, sqid, summary string) error {
	return s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		run, err := q.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		t, err := s.Task(ctx, q, sqid)
		if err != nil {
			return err
		}
		in, err := q.IsAncestorOrSelf(ctx, dbgen.IsAncestorOrSelfParams{NodeID: t.ID, AncestorID: run.TaskID})
		if err != nil {
			return err
		}
		if in != 1 {
			return fmt.Errorf("%w: %s is outside run %d's subtree", errs.Forbidden, sqid, runID)
		}
		if kids, err := hasChildren(ctx, q, t.ID); err != nil {
			return err
		} else if kids {
			return invalid("%s has subtasks; complete those", sqid)
		}
		if t.State != "open" && t.State != "in_progress" {
			return conflict("%s is %s", sqid, t.State)
		}
		if len(summary) > 2000 {
			summary = summary[:2000]
		}
		if err := q.CompleteTask(ctx, dbgen.CompleteTaskParams{CompletionSummary: summary, CompletedRunID: nint(runID), ID: t.ID}); err != nil {
			return err
		}
		if err := s.event(ctx, q, sqid, AgentActor(runID), "complete", map[string]any{"summary": summary}); err != nil {
			return err
		}
		return recompute(ctx, q, t.ParentID)
	})
}

// Recompute refreshes container states above sqid (after an out-of-band leaf change).
func (s *Service) Recompute(ctx context.Context, sqid string) error {
	return s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		t, err := s.Task(ctx, q, sqid)
		if err != nil {
			return err
		}
		return recompute(ctx, q, t.ParentID)
	})
}

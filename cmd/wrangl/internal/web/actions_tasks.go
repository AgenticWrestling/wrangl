package web

// Actions: actions on tasks, questions, pull requests and runs.

import (
	"database/sql"
	"fmt"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/reporesolve"
	"net/url"
	"strconv"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

func actTaskNew(h *Handler, a *actReq) (res actResult, err error) {
	var t dbgen.Task
	pos := stack.Position{First: a.q.Get("first") == "1", After: a.q.Get("after")}
	t, err = h.D.Stack.Create(a.ctx, stack.CreateParams{Project: a.q.Get("project"), Parent: a.q.Get("parent"), Title: a.str("title"), Prompt: a.str("prompt"), Type: a.str("ttype"), TargetDate: a.str("ttarget"), Pos: pos, Actor: stack.User})
	if err == nil {
		res.msg = "created " + t.Sqid.String
		res.reset = map[string]any{"title": "", "prompt": "", "queue": false, "ttarget": ""}
		if a.q.Get("parent") == "" { // the backlog sheet's type picker (subtasks inherit instead)
			res.reset["ttype"] = "code"
		}
		if a.flag("queue") {
			if _, qerr := h.D.Stack.Queue(a.ctx, t.Sqid.String); qerr != nil {
				res.msg += " (not queued: " + qerr.Error() + ")"
			} else {
				res.msg += " and queued"
			}
		}
	}
	return
}

func actTaskEdit(h *Handler, a *actReq) (res actResult, err error) {
	title, prompt := a.str("etitle"), a.str("eprompt")
	if _, err = h.D.Stack.Edit(a.ctx, a.sqid, &title, &prompt, stack.User); err == nil && a.str("etype") != "" {
		_, err = h.D.Stack.SetType(a.ctx, a.sqid, a.str("etype"), stack.User)
	}
	if err == nil {
		_, err = h.D.Stack.SetTargetDate(a.ctx, a.sqid, a.str("etarget"), stack.User)
	}
	if _, ok := a.sig["erepo"]; err == nil && ok { // only sent for a project with several repos
		_, err = h.D.Stack.SetRepo(a.ctx, a.sqid, a.str("erepo"), stack.User)
	}
	res.msg = "saved"
	return
}

func actTaskAsk(h *Handler, a *actReq) (res actResult, err error) {
	question, field := a.str("askq"), "askq"
	if a.q.Get("reply") == "1" {
		question, field = a.str("replyq"), "replyq"
	}
	if _, err = h.D.Agents.Ask(a.ctx, a.sqid, a.str("askagent"), question); err == nil {
		res.msg = "asked; the answer will appear on the task"
		res.reset = map[string]any{field: ""}
	}
	return
}

func actProposalApply(h *Handler, a *actReq) (res actResult, err error) {
	n, _ := strconv.Atoi(a.q.Get("n"))
	if err = h.D.Agents.Apply(a.ctx, a.id, n); err == nil {
		res.msg = "done"
	}
	return
}

func actTaskSplit(h *Handler, a *actReq) (res actResult, err error) {
	var nodes []stack.SplitNode
	if nodes, err = stack.ParseOutline(a.str("outline")); err == nil {
		_, err = h.D.Stack.Split(a.ctx, a.sqid, nodes, a.flag("chain"), a.flag("copy"), stack.User)
		res.msg = fmt.Sprintf("split into %d subtasks", len(nodes))
		res.reset = map[string]any{"outline": ""}
	}
	return
}

func actTaskPlan(h *Handler, a *actReq) (res actResult, err error) {
	res.back = "/projects"
	_, err = h.D.Plans.Queue(a.ctx, a.sqid, a.str("guidance"))
	res.msg = "planning… the proposal will arrive in Inbox"
	return
}

func actTaskQueue(h *Handler, a *actReq) (res actResult, err error) {
	res.back = "/projects"
	// The queue sheet of a project with several repos lets the owner choose the repo first.
	if repo, ok := a.sig["qrepo"].(string); ok && strings.TrimSpace(repo) != "" {
		t, terr := h.D.Stack.Task(a.ctx, h.D.Fleet.DB.RQ, a.sqid)
		if terr != nil {
			return res, terr
		}
		eff, _ := reporesolve.Resolve(a.ctx, h.D.Fleet.DB.RQ, t.ID)
		if cur := reporesolve.Name(a.ctx, h.D.Fleet.DB.RQ, eff); cur != strings.TrimSpace(repo) { // the choice differs from what it already resolves to
			if _, err = h.D.Stack.SetRepo(a.ctx, a.sqid, strings.TrimSpace(repo), stack.User); err != nil {
				return res, err
			}
		}
	}
	_, err = h.D.Plans.QueueWork(a.ctx, a.sqid)
	res.msg, res.flash = "queued", a.sqid
	return
}

func actTaskDequeue(h *Handler, a *actReq) (res actResult, err error) {
	_, err = h.D.Runs.Dequeue(a.ctx, a.sqid)
	res.msg, res.flash = "back in the backlog", a.sqid
	return
}

// actQueueMove is up/down on a queued row: it swaps the run's rank with the neighbour's.
func actQueueMove(h *Handler, a *actReq) (res actResult, err error) {
	run, rerr := h.D.Fleet.DB.RQ.GetRun(a.ctx, a.id)
	if rerr != nil {
		return res, rerr
	}
	task, _ := h.D.Fleet.DB.RQ.GetTask(a.ctx, run.TaskID)
	proj, _ := h.D.Fleet.DB.RQ.GetProject(a.ctx, task.ProjectID)
	secs, terr := h.tree(a.ctx, proj.Slug, false)
	if terr != nil {
		return res, terr
	}
	var ids []int64
	at := -1
	for i, e := range secs.Queue {
		ids = append(ids, e.RunID)
		if e.RunID == a.id {
			at = i
		}
	}
	to := at - 1
	if a.q.Get("dir") == "down" {
		to = at + 1
	}
	if at < 0 || to < 0 || to >= len(ids) {
		return res, fmt.Errorf("%w: nothing to move", errs.Conflict)
	}
	ids[at], ids[to] = ids[to], ids[at]
	for i, rid := range ids { // persist the whole order so ties (unranked runs) stay put
		if _, err = h.D.Fleet.DB.Q.SetRunQueueRank(a.ctx, dbgen.SetRunQueueRankParams{QueueRank: sql.NullFloat64{Float64: float64(i + 1), Valid: true}, ID: rid}); err != nil {
			return
		}
	}
	res.msg, res.flash = "moved "+a.q.Get("dir"), a.sqid
	return
}

// actQueueAll queues every open top-level task of the project that is not already queued or running.
func actQueueAll(h *Handler, a *actReq) (res actResult, err error) {
	secs, terr := h.tree(a.ctx, a.q.Get("project"), false)
	if terr != nil {
		return res, terr
	}
	n, skipped := 0, 0
	for _, nd := range secs.Backlog {
		if nd.State != "open" {
			continue
		}
		if _, qerr := h.D.Plans.QueueWork(a.ctx, nd.Sqid); qerr != nil {
			skipped++
			continue
		}
		n++
	}
	res.msg = fmt.Sprintf("queued %d", n)
	if skipped > 0 {
		res.msg += fmt.Sprintf(" (%d skipped: locked or not queueable)", skipped)
	}
	return
}

func actTaskState(h *Handler, a *actReq) (res actResult, err error) {
	op := strings.TrimPrefix(a.name, "task-")
	switch op {
	case "complete":
		err = h.D.Stack.Complete(a.ctx, a.sqid, "", stack.User)
	case "reopen":
		err = h.D.Stack.Reopen(a.ctx, a.sqid, stack.User)
	case "cancel":
		err = h.D.Stack.Cancel(a.ctx, a.sqid, "", stack.User)
	}
	res.msg = op + "d"
	return
}

func actTaskDelete(h *Handler, a *actReq) (res actResult, err error) {
	t, terr := h.D.Stack.Task(a.ctx, h.D.Fleet.DB.RQ, a.sqid)
	if err = terr; err == nil {
		p, _ := h.D.Fleet.DB.RQ.GetProject(a.ctx, t.ProjectID)
		err = h.D.Stack.Delete(a.ctx, a.sqid, stack.User)
		res.msg, res.nav = "deleted", "/projects/"+url.PathEscape(p.Slug)
	}
	return
}

func actTaskNeeds(h *Handler, a *actReq) (res actResult, err error) {
	err = h.D.Stack.NeedsAdd(a.ctx, a.sqid, a.str("precursor"), stack.User)
	res.msg = "precursor added"
	return
}

func actAnswer(h *Handler, a *actReq) (res actResult, err error) {
	res.back = "/inbox"
	err = h.D.Runs.Answer(a.ctx, a.id, a.q.Get("value"), a.str(fmt.Sprintf("ans%d", a.id)))
	res.msg = "answered"
	res.dropEl = fmt.Sprintf("q%d", a.id)
	if a.q.Get("value") == "edit" { // plan proposal: open the split sheet with it
		if qv, qerr := h.D.Runs.Question(a.ctx, a.id); qerr == nil {
			res.nav = fmt.Sprintf("/task/%s?split=%d#split", qv.Task, a.id)
		}
	}
	return
}

func actPR(h *Handler, a *actReq) (res actResult, err error) {
	res.back = "/inbox"
	switch a.q.Get("op") {
	case "approve":
		err = h.D.PRs.Approve(a.ctx, a.id)
		res.msg = "Merged ✓ the task is done"
	case "request-changes":
		err = h.D.PRs.RequestChanges(a.ctx, a.id, a.str(fmt.Sprintf("pr%d", a.id)))
		res.msg = "Changes requested: a revise run is queued"
	case "close":
		err = h.D.PRs.Close(a.ctx, a.id)
		res.msg = "PR closed"
	default:
		err = fmt.Errorf("%w: unknown pr op %q", errs.Invalid, a.q.Get("op"))
	}
	res.dropEl = fmt.Sprintf("pr-%d", a.id) // gone from the Inbox now, not at the next live patch
	return
}

func actPRResolve(h *Handler, a *actReq) (res actResult, err error) {
	res.back = "/inbox"
	err = h.D.PRs.ResolveConflicts(a.ctx, a.id)
	res.msg = "Smart Merge queued: an agent will resolve the conflicts"
	return
}

func actRunCancel(h *Handler, a *actReq) (res actResult, err error) {
	err = h.D.Runs.Cancel(a.ctx, a.id)
	res.msg = "cancelling"
	return
}

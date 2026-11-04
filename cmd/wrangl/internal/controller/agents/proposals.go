package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

// Proposal is an action an agent suggests; the owner approves it with one click. Actions are a
// closed set run in-process through the same services the UI uses, so arguments are checked
// and nothing is shelled out.
type Proposal struct {
	Label       string         `json:"label"`
	Description string         `json:"description,omitempty"`
	Action      string         `json:"action"`
	Args        map[string]any `json:"args"`
	State       string         `json:"state,omitempty"` // "" (pending) | done | error
	Result      string         `json:"result,omitempty"`
}

// MaxProposals is how many one-click options a reply can carry.
const MaxProposals = 5

// Actions is the allowlist and the schema the agent is told about.
var Actions = map[string]string{
	"task.create":      `{"title","prompt"?,"parent"? (sqid; omit = top level),"project"? (slug; default this task's),"type"?,"after"?|"before"? (sibling sqid),"needs"? [sqid]}`,
	"task.edit":        `{"sqid","title"?,"prompt"?}`,
	"task.move":        `{"sqid","parent"? (sqid, or "" for top level; omit = keep),"after"?|"before"? (sibling sqid)|"first"? true}`,
	"task.split":       `{"sqid","titles": [..],"chain"? bool}`,
	"task.type":        `{"sqid","type"}`,
	"task.target_date": `{"sqid","date" (YYYY-MM-DD, "" clears)}`,
	"task.needs_add":   `{"sqid","precursor" (sqid)}`,
	"task.needs_rm":    `{"sqid","precursor" (sqid)}`,
	"task.queue":       `{"sqid"}`,
	"task.complete":    `{"sqid","summary"?}`,
	"task.reopen":      `{"sqid"}`,
}

const fence = "```wrangl-proposals"

// extractProposals pulls the last fenced wrangl-proposals block out of a reply. It returns the
// text without the block and the valid proposals (at most MaxProposals); a malformed block is
// dropped and the text is returned unchanged apart from removing it.
func extractProposals(text string) (string, []Proposal) {
	i := strings.LastIndex(text, fence)
	if i < 0 {
		return text, nil
	}
	rest := text[i+len(fence):]
	j := strings.Index(rest, "```")
	if j < 0 {
		return text, nil
	}
	clean := strings.TrimSpace(text[:i] + rest[j+3:])
	var raw []Proposal
	if json.Unmarshal([]byte(strings.TrimSpace(rest[:j])), &raw) != nil {
		return clean, nil
	}
	var out []Proposal
	for _, p := range raw {
		p.Label = strings.TrimSpace(p.Label)
		if _, ok := Actions[p.Action]; !ok || p.Label == "" || len(out) == MaxProposals {
			continue
		}
		if p.Args == nil {
			p.Args = map[string]any{}
		}
		p.State, p.Result = "", ""
		out = append(out, p)
	}
	return clean, out
}

// ParseProposals decodes a stored proposals column.
func ParseProposals(s string) []Proposal {
	var ps []Proposal
	json.Unmarshal([]byte(s), &ps)
	return ps
}

// Apply runs proposal n of a thread message as the user. A proposal runs once; the outcome is
// recorded on it (and returned as the error, if any).
func (s *Service) Apply(ctx context.Context, msgID int64, n int) error {
	m, err := s.DB.RQ.GetThreadMessage(ctx, msgID)
	if err != nil {
		return fmt.Errorf("%w: message %d", errs.NotFound, msgID)
	}
	ps := ParseProposals(m.Proposals)
	if n < 0 || n >= len(ps) {
		return fmt.Errorf("%w: no such option", errs.NotFound)
	}
	if ps[n].State == "done" {
		return fmt.Errorf("%w: already applied", errs.Conflict)
	}
	task, err := s.DB.RQ.GetTask(ctx, m.TaskID)
	if err != nil {
		return err
	}
	if cur, err := s.DB.RQ.SubtreeRev(ctx, m.TaskID); err == nil && m.Rev != "" && cur != m.Rev {
		return fmt.Errorf("%w: the tasks changed since this was suggested; ask again", errs.Conflict)
	}
	proj, _ := s.DB.RQ.GetProject(ctx, task.ProjectID)
	res, err := s.run(ctx, ps[n], proj.Slug)
	if err != nil {
		ps[n].State, ps[n].Result = "error", err.Error()
	} else {
		ps[n].State, ps[n].Result = "done", res
	}
	b, _ := json.Marshal(ps)
	if e := s.DB.Q.SetThreadProposals(ctx, dbgen.SetThreadProposalsParams{ID: msgID, Proposals: string(b)}); e != nil {
		return e
	}
	if err == nil { // our own change must not expire the sibling options
		s.stamp(ctx, m.TaskID, msgID)
	}
	s.Bus.Publish("thread", task.Sqid.String)
	s.Bus.Publish("task", task.Sqid.String)
	return err
}

func (s *Service) run(ctx context.Context, p Proposal, project string) (string, error) {
	a := argv(p.Args)
	st, u := s.Stack, stack.User
	pos := stack.Position{First: a.bool("first"), After: a.str("after"), Before: a.str("before")}
	switch p.Action {
	case "task.create":
		prj := a.str("project")
		if prj == "" {
			prj = project
		}
		t, err := st.Create(ctx, stack.CreateParams{Project: prj, Parent: a.str("parent"), Title: a.str("title"), Prompt: a.str("prompt"),
			Type: a.str("type"), Needs: a.strs("needs"), Pos: pos, Actor: u})
		if err != nil {
			return "", err
		}
		return "created " + t.Sqid.String, nil
	case "task.edit":
		var title, prompt *string
		if a.has("title") {
			v := a.str("title")
			title = &v
		}
		if a.has("prompt") {
			v := a.str("prompt")
			prompt = &v
		}
		_, err := st.Edit(ctx, a.str("sqid"), title, prompt, u)
		return "edited", err
	case "task.move":
		var np *string
		if a.has("parent") {
			v := a.str("parent")
			np = &v
		}
		_, err := st.Move(ctx, a.str("sqid"), np, pos, u)
		return "moved", err
	case "task.split":
		var nodes []stack.SplitNode
		for _, t := range a.strs("titles") {
			nodes = append(nodes, stack.SplitNode{Title: t})
		}
		if len(nodes) == 0 {
			return "", fmt.Errorf("%w: no titles", errs.Invalid)
		}
		_, err := st.Split(ctx, a.str("sqid"), nodes, a.bool("chain"), false, u)
		return fmt.Sprintf("split into %d", len(nodes)), err
	case "task.type":
		_, err := st.SetType(ctx, a.str("sqid"), a.str("type"), u)
		return "type set", err
	case "task.target_date":
		_, err := st.SetTargetDate(ctx, a.str("sqid"), a.str("date"), u)
		return "date set", err
	case "task.needs_add":
		return "added", st.NeedsAdd(ctx, a.str("sqid"), a.str("precursor"), u)
	case "task.needs_rm":
		return "removed", st.NeedsRm(ctx, a.str("sqid"), a.str("precursor"), u)
	case "task.queue":
		if s.Queue == nil {
			return "", fmt.Errorf("%w: queueing is unavailable", errs.Invalid)
		}
		return "queued", s.Queue(ctx, a.str("sqid"))
	case "task.complete":
		return "completed", st.Complete(ctx, a.str("sqid"), a.str("summary"), u)
	case "task.reopen":
		return "reopened", st.Reopen(ctx, a.str("sqid"), u)
	}
	return "", fmt.Errorf("%w: unknown action %q", errs.Invalid, p.Action)
}

type argv map[string]any

func (a argv) has(k string) bool { _, ok := a[k]; return ok }
func (a argv) str(k string) string {
	v, _ := a[k].(string)
	return strings.TrimSpace(v)
}
func (a argv) bool(k string) bool { v, _ := a[k].(bool); return v }
func (a argv) strs(k string) []string {
	var out []string
	if l, ok := a[k].([]any); ok {
		for _, v := range l {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	}
	return out
}

// schemaHelp documents proposals for the agent's system prompt.
func schemaHelp() string {
	var b strings.Builder
	b.WriteString("\n\nProposing actions: when you recommend changes, end your reply with ONE fenced block the owner can approve with a click each (at most 5; label them so they read as buttons):\n\n" + fence + "\n[{\"label\":\"Split Child A in three\",\"description\":\"optional one line\",\"action\":\"task.split\",\"args\":{\"sqid\":\"abc123\",\"titles\":[\"a\",\"b\",\"c\"]}}]\n```\n\nActions and their args (sqids are the short ids shown in the tree):\n")
	for _, k := range []string{"task.create", "task.edit", "task.move", "task.split", "task.type", "task.target_date", "task.needs_add", "task.needs_rm", "task.queue", "task.complete", "task.reopen"} {
		fmt.Fprintf(&b, "- %s %s\n", k, Actions[k])
	}
	b.WriteString("Only propose what you did not already do yourself. Anything not in this list (delete, cancel, merge, terminate) can only be described in prose.")
	return b.String()
}

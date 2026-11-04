package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

// Project chat: the owner talks to a controller agent about a whole project. It shares everything
// with task asks (agent registry, process boxing and slot cap, session resume, proposals and their
// one-click Apply) and differs in scope: the context is the project's tasks, runs, pull requests
// and repos, and the rows live in project_thread. See docs/wrangl/adr/0005.

// ProjectContext is what the agent was given, summarised for the UI's context chip.
type ProjectContext struct {
	Tasks   int  `json:"tasks"`
	Runs    int  `json:"runs"`
	PRs     int  `json:"prs"`
	Repos   int  `json:"repos"`
	Trimmed bool `json:"trimmed,omitempty"`
}

// TaskLink is a task id mentioned in a message, so the UI can link it to its row.
type TaskLink struct {
	Sqid  string `json:"sqid"`
	Title string `json:"title"`
	State string `json:"state"`
}

// ProjectMessage is one message of a project chat.
type ProjectMessage struct {
	ID        int64      `json:"id"`
	Role      string     `json:"role"`
	Body      string     `json:"body"`
	Status    string     `json:"status"`
	At        string     `json:"at"`
	Agent     string     `json:"agent,omitempty"`
	Proposals []Proposal `json:"proposals"`
	Tasks     []TaskLink `json:"tasks"` // task ids found in Body, in order of appearance
}

const projectSystemPrompt = `You are a wrangl controller agent, talking with the owner about one whole project: its tasks, runs, pull requests and repos. The project brief below the question header is current as of now.

Rules:
- Be concise and concrete. Refer to tasks by their short id in backticks (for example ` + "`abc123`" + `) so the UI can link them.
- Use the wrangl CLI (see the wrangl skill) when you need more than the brief. Prefer ` + "`-o json`" + `.
- Prefer proposing changes over making them: end with one proposals block (see below) the owner approves with a click. Do not delete, cancel, terminate, approve or merge anything: describe it instead.
- You run on the controller host, which must stay responsive. Do not build, compile, run tests or start long processes. Reading files is fine.`

func (s *Service) projectID(ctx context.Context, slug string) (dbgen.Project, error) {
	p, err := s.DB.RQ.GetProjectBySlug(ctx, slug)
	if err == sql.ErrNoRows {
		return p, fmt.Errorf("%w: project %q", errs.NotFound, slug)
	}
	if err == nil && p.ArchivedAt.Valid {
		return p, fmt.Errorf("%w: project %q is archived", errs.Conflict, slug)
	}
	return p, err
}

// AskProject records a message in the project's chat and starts the agent in the background.
// "/queue <task>" and "/dequeue <task>" are handled here, without the model.
func (s *Service) AskProject(ctx context.Context, slug, agentName, question string) (ProjectMessage, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return ProjectMessage{}, fmt.Errorf("%w: ask something", errs.Invalid)
	}
	p, err := s.projectID(ctx, slug)
	if err != nil {
		return ProjectMessage{}, err
	}
	if strings.HasPrefix(question, "/") {
		return s.slash(ctx, p, question)
	}
	ag, err := s.agent(ctx, agentName)
	if err != nil {
		return ProjectMessage{}, err
	}
	var session string
	s.DB.R.QueryRowContext(ctx, `SELECT session_id FROM project_thread WHERE project_id = ? AND agent_id = ? AND session_id <> '' ORDER BY id DESC LIMIT 1`, p.ID, ag.ID).Scan(&session)
	brief, _, err := s.projectBrief(ctx, p)
	if err != nil {
		return ProjectMessage{}, err
	}
	if _, err := s.addProjectMsg(ctx, p.ID, "user", question, "done", 0); err != nil {
		return ProjectMessage{}, err
	}
	aid, err := s.addProjectMsg(ctx, p.ID, "agent", "", "running", ag.ID)
	if err != nil {
		return ProjectMessage{}, err
	}
	s.Bus.Publish("thread", "project:"+slug)
	go s.answerProject(context.WithoutCancel(ctx), p, aid, ag, session, brief+"\n\n## Question\n\n"+question)
	return ProjectMessage{ID: aid, Role: "agent", Status: "running", Proposals: []Proposal{}, Tasks: []TaskLink{}}, nil
}

func (s *Service) addProjectMsg(ctx context.Context, projectID int64, role, body, status string, agentID int64) (int64, error) {
	var ag sql.NullInt64
	if agentID != 0 {
		ag = sql.NullInt64{Int64: agentID, Valid: true}
	}
	res, err := s.DB.W.ExecContext(ctx, `INSERT INTO project_thread (project_id, role, body, agent_id, status) VALUES (?, ?, ?, ?, ?)`, projectID, role, body, ag, status)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Service) answerProject(ctx context.Context, p dbgen.Project, msgID int64, ag dbgen.ControllerAgent, session, prompt string) {
	defer s.Bus.Publish("thread", "project:"+p.Slug)
	res, failBody, failed := s.execute(ctx, ag, session, prompt, projectSystemPrompt+schemaHelp(), "project:"+p.Slug)
	if failed {
		s.finishProject(ctx, msgID, failBody, "error", res.SessionID, nil, "")
		return
	}
	text, props := extractProposals(res.Text)
	s.finishProject(ctx, msgID, text, "done", res.SessionID, props, s.projectRev(ctx, p.ID))
}

func (s *Service) finishProject(ctx context.Context, id int64, body, status, session string, props []Proposal, rev string) {
	pj, _ := json.Marshal(props)
	if len(props) == 0 {
		pj = []byte("[]")
	}
	if _, err := s.DB.W.ExecContext(ctx, `UPDATE project_thread SET body = ?, status = ?, session_id = ?, proposals = ?, rev = ? WHERE id = ?`, body, status, session, string(pj), rev, id); err != nil {
		s.Log.Error("finish project message", "err", err)
	}
}

// projectRev fingerprints what proposals depend on (tasks and the live runs of the project); a
// proposal made against an older fingerprint is refused, like task proposals.
func (s *Service) projectRev(ctx context.Context, projectID int64) string {
	var rev string
	s.DB.R.QueryRowContext(ctx, `SELECT
 (SELECT COALESCE(MAX(updated_at), '') || '/' || COUNT(*) FROM task WHERE project_id = ?1) || '|' ||
 (SELECT COALESCE(group_concat(r.id || r.state || COALESCE(r.queue_rank, ''), ','), '') FROM run r JOIN task t ON t.id = r.task_id
   WHERE t.project_id = ?1 AND r.state NOT IN ('succeeded', 'failed', 'cancelled'))`, projectID).Scan(&rev)
	return rev
}

// ProjectThread is the project's chat with the context chip for the next question.
func (s *Service) ProjectThread(ctx context.Context, slug string) ([]ProjectMessage, ProjectContext, error) {
	p, err := s.DB.RQ.GetProjectBySlug(ctx, slug)
	if err != nil {
		return nil, ProjectContext{}, fmt.Errorf("%w: project %q", errs.NotFound, slug)
	}
	_, chip, err := s.projectBrief(ctx, p)
	if err != nil {
		return nil, chip, err
	}
	known, err := s.projectTasks(ctx, p.ID)
	if err != nil {
		return nil, chip, err
	}
	rows, err := s.DB.R.QueryContext(ctx, `SELECT m.id, m.role, m.body, m.status, m.at, COALESCE(a.name, ''), m.proposals
FROM project_thread m LEFT JOIN controller_agent a ON a.id = m.agent_id WHERE m.project_id = ? ORDER BY m.id`, p.ID)
	if err != nil {
		return nil, chip, err
	}
	defer rows.Close()
	out := []ProjectMessage{}
	for rows.Next() {
		var m ProjectMessage
		var props string
		if err := rows.Scan(&m.ID, &m.Role, &m.Body, &m.Status, &m.At, &m.Agent, &props); err != nil {
			return nil, chip, err
		}
		m.Proposals = ParseProposals(props)
		if m.Proposals == nil {
			m.Proposals = []Proposal{}
		}
		m.Tasks = mentions(m.Body, known)
		out = append(out, m)
	}
	return out, chip, rows.Err()
}

var tokenRE = regexp.MustCompile(`[0-9A-Za-z]{3,16}`)

// mentions lists the project's tasks whose short ids appear in text, in order of first appearance.
func mentions(text string, known map[string]TaskLink) []TaskLink {
	out := []TaskLink{}
	seen := map[string]bool{}
	for _, tok := range tokenRE.FindAllString(text, -1) {
		if tl, ok := known[tok]; ok && !seen[tok] {
			seen[tok] = true
			out = append(out, tl)
		}
	}
	return out
}

func (s *Service) projectTasks(ctx context.Context, projectID int64) (map[string]TaskLink, error) {
	rows, err := s.DB.R.QueryContext(ctx, `SELECT COALESCE(sqid, ''), title, state FROM task WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]TaskLink{}
	for rows.Next() {
		var t TaskLink
		if err := rows.Scan(&t.Sqid, &t.Title, &t.State); err != nil {
			return nil, err
		}
		out[t.Sqid] = t
	}
	return out, rows.Err()
}

// projectBrief is the project as markdown for the agent, trimmed to maxContext, plus the chip.
func (s *Service) projectBrief(ctx context.Context, p dbgen.Project) (string, ProjectContext, error) {
	var b strings.Builder
	var chip ProjectContext
	fmt.Fprintf(&b, "## Project %s (%s)\n\n%s\n", p.Slug, p.Name, strings.TrimSpace(p.Description))
	repos, err := s.DB.RQ.ListRepos(ctx, p.ID)
	if err != nil {
		return "", chip, err
	}
	b.WriteString("\n## Repos\n")
	for _, r := range repos {
		fmt.Fprintf(&b, "- %s/%s\n", r.Repo.Owner, r.Repo.Name)
		chip.Repos++
	}
	// Runs worth knowing about: running and queued ones, queue in rank order.
	rr, err := s.DB.R.QueryContext(ctx, `SELECT r.id, r.state, r.kind, COALESCE(t.sqid, ''), t.title, COALESCE(ru.name, ''), r.queue_rank
FROM run r JOIN task t ON t.id = r.task_id LEFT JOIN runner ru ON ru.id = r.runner_id
WHERE t.project_id = ? AND r.state NOT IN ('succeeded', 'failed', 'cancelled')
ORDER BY (r.state = 'queued'), (r.queue_rank IS NULL), r.queue_rank, r.id`, p.ID)
	if err != nil {
		return "", chip, err
	}
	b.WriteString("\n## Runs (running first, then the queue in order)\n")
	for rr.Next() {
		var id int64
		var state, kind, sqid, title, runner string
		var rank sql.NullFloat64
		if err := rr.Scan(&id, &state, &kind, &sqid, &title, &runner, &rank); err != nil {
			rr.Close()
			return "", chip, err
		}
		chip.Runs++
		fmt.Fprintf(&b, "- run %d [%s %s] `%s` %s%s\n", id, state, kind, sqid, title, map[bool]string{true: " on " + runner}[runner != ""])
	}
	rr.Close()
	prs, err := s.DB.R.QueryContext(ctx, `SELECT pr.gh_number, pr.url, COALESCE(t.sqid, ''), t.title FROM pull_request pr JOIN task t ON t.id = pr.task_id
WHERE t.project_id = ? AND pr.state = 'open' ORDER BY pr.id DESC`, p.ID)
	if err != nil {
		return "", chip, err
	}
	b.WriteString("\n## Open pull requests\n")
	for prs.Next() {
		var n int64
		var url, sqid, title string
		if err := prs.Scan(&n, &url, &sqid, &title); err != nil {
			prs.Close()
			return "", chip, err
		}
		chip.PRs++
		fmt.Fprintf(&b, "- #%d %s for `%s` %s\n", n, url, sqid, title)
	}
	prs.Close()
	ts, err := s.DB.R.QueryContext(ctx, `SELECT COALESCE(t.sqid, ''), t.title, t.state, t.task_type, COALESCE(pt.sqid, ''), t.prompt
FROM task t LEFT JOIN task pt ON pt.id = t.parent_id WHERE t.project_id = ? AND t.state NOT IN ('done', 'cancelled') ORDER BY t.id`, p.ID)
	if err != nil {
		return "", chip, err
	}
	b.WriteString("\n## Open tasks (id, state, type, parent)\n")
	for ts.Next() {
		var sqid, title, state, typ, parent, prompt string
		if err := ts.Scan(&sqid, &title, &state, &typ, &parent, &prompt); err != nil {
			ts.Close()
			return "", chip, err
		}
		chip.Tasks++
		if b.Len() > maxContext {
			chip.Trimmed = true
			continue // keep counting for the chip
		}
		fmt.Fprintf(&b, "- `%s` [%s, %s] **%s**%s\n", sqid, state, typ, title, map[bool]string{true: " (child of `" + parent + "`)"}[parent != ""])
		if prompt = strings.TrimSpace(prompt); prompt != "" && b.Len() < maxContext/2 {
			if len(prompt) > 300 {
				prompt = prompt[:300] + "…"
			}
			fmt.Fprintf(&b, "  > %s\n", strings.ReplaceAll(prompt, "\n", " "))
		}
	}
	ts.Close()
	if chip.Trimmed {
		b.WriteString("\n…(task list truncated; use `wrangl task list` for the rest)\n")
	}
	return b.String(), chip, nil
}

// ApplyProject runs proposal n of a project chat message as the user (once).
func (s *Service) ApplyProject(ctx context.Context, msgID int64, n int) error {
	var projectID int64
	var props, rev string
	err := s.DB.R.QueryRowContext(ctx, `SELECT project_id, proposals, rev FROM project_thread WHERE id = ?`, msgID).Scan(&projectID, &props, &rev)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: message %d", errs.NotFound, msgID)
	} else if err != nil {
		return err
	}
	ps := ParseProposals(props)
	if n < 0 || n >= len(ps) {
		return fmt.Errorf("%w: no such option", errs.NotFound)
	}
	if ps[n].State == "done" {
		return fmt.Errorf("%w: already applied", errs.Conflict)
	}
	if rev != "" && s.projectRev(ctx, projectID) != rev {
		return fmt.Errorf("%w: the project changed since this was suggested; ask again", errs.Conflict)
	}
	proj, err := s.DB.RQ.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	res, runErr := s.run(ctx, ps[n], proj.Slug)
	if runErr != nil {
		ps[n].State, ps[n].Result = "error", runErr.Error()
	} else {
		ps[n].State, ps[n].Result = "done", res
	}
	b, _ := json.Marshal(ps)
	newRev := rev
	if runErr == nil { // our own change must not expire the sibling options
		newRev = s.projectRev(ctx, projectID)
	}
	if _, e := s.DB.W.ExecContext(ctx, `UPDATE project_thread SET proposals = ?, rev = ? WHERE id = ?`, string(b), newRev, msgID); e != nil {
		return e
	}
	s.Bus.Publish("thread", "project:"+proj.Slug)
	s.Bus.Publish("task", proj.Slug)
	return runErr
}

// reorderQueue ranks the project's queued runs in the order of sqids (tasks without a queued run are an error).
func (s *Service) reorderQueue(ctx context.Context, project string, sqids []string) (string, error) {
	if len(sqids) == 0 {
		return "", fmt.Errorf("%w: no tasks to order", errs.Invalid)
	}
	known := map[string]int64{}
	rows, err := s.DB.R.QueryContext(ctx, `SELECT COALESCE(t.sqid, ''), r.id FROM run r JOIN task t ON t.id = r.task_id JOIN project p ON p.id = t.project_id
WHERE p.slug = ? AND r.state = 'queued'`, project)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var sq string
		var id int64
		rows.Scan(&sq, &id)
		known[sq] = id
	}
	rows.Close()
	ids := make([]int64, 0, len(sqids))
	for _, sq := range sqids {
		id, ok := known[sq]
		if !ok {
			return "", fmt.Errorf("%w: %s is not queued in this project", errs.Conflict, sq)
		}
		ids = append(ids, id)
	}
	for i, id := range ids {
		if _, err := s.DB.W.ExecContext(ctx, `UPDATE run SET queue_rank = ? WHERE id = ? AND state = 'queued'`, float64(i+1), id); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("ordered %d queued tasks", len(ids)), nil
}

// slash handles "/queue <task>" and "/dequeue <task>" without the model. <task> is a short id or a
// unique part of a title; when it matches several tasks the reply offers one button per match.
func (s *Service) slash(ctx context.Context, p dbgen.Project, line string) (ProjectMessage, error) {
	verb, ref, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	ref = strings.TrimSpace(ref)
	action := map[string]string{"queue": "task.queue", "dequeue": "task.dequeue"}[strings.ToLower(verb)]
	if action == "" {
		return ProjectMessage{}, fmt.Errorf("%w: unknown command /%s (try /queue <task> or /dequeue <task>)", errs.Invalid, verb)
	}
	if ref == "" {
		return ProjectMessage{}, fmt.Errorf("%w: /%s needs a task id or part of its title", errs.Invalid, verb)
	}
	known, err := s.projectTasks(ctx, p.ID)
	if err != nil {
		return ProjectMessage{}, err
	}
	var matches []TaskLink
	if tl, ok := known[ref]; ok {
		matches = []TaskLink{tl}
	} else {
		for _, tl := range known {
			if tl.State != "done" && tl.State != "cancelled" && strings.Contains(strings.ToLower(tl.Title), strings.ToLower(ref)) {
				matches = append(matches, tl)
			}
		}
		sort.Slice(matches, func(i, j int) bool { return matches[i].Sqid < matches[j].Sqid })
	}
	if _, err := s.addProjectMsg(ctx, p.ID, "user", line, "done", 0); err != nil {
		return ProjectMessage{}, err
	}
	var body string
	var props []Proposal
	switch {
	case len(matches) == 0:
		body = fmt.Sprintf("No open task matches %q in this project.", ref)
	case len(matches) == 1:
		do := s.Queue
		if action == "task.dequeue" {
			do = s.Dequeue
		}
		if do == nil {
			body = "That is unavailable on this controller."
		} else if err := do(ctx, matches[0].Sqid); err != nil {
			body = fmt.Sprintf("Could not /%s `%s`: %s", verb, matches[0].Sqid, err)
		} else {
			body = fmt.Sprintf("Done: /%s `%s` %s.", verb, matches[0].Sqid, matches[0].Title)
		}
	default:
		body = fmt.Sprintf("%q matches %d tasks; pick one:", ref, len(matches))
		for _, m := range matches {
			if len(props) == MaxProposals {
				break
			}
			props = append(props, Proposal{Label: strings.Title(verb) + " " + m.Sqid + " " + m.Title, Action: action, Args: map[string]any{"sqid": m.Sqid}})
		}
	}
	id, err := s.addProjectMsg(ctx, p.ID, "agent", "", "done", 0)
	if err != nil {
		return ProjectMessage{}, err
	}
	s.finishProject(ctx, id, body, "done", "", props, s.projectRev(ctx, p.ID))
	s.Bus.Publish("thread", "project:"+p.Slug)
	s.Bus.Publish("task", p.Slug)
	if props == nil {
		props = []Proposal{}
	}
	return ProjectMessage{ID: id, Role: "agent", Body: body, Status: "done", Proposals: props, Tasks: mentions(body, known)}, nil
}

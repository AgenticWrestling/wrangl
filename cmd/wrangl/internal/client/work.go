package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/issuestore"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func esc(s string) string { return url.PathEscape(s) }

// --- projects ---

func (c *Client) Projects(ctx context.Context) ([]proto.ProjectView, error) {
	return c.ProjectsFiltered(ctx, "")
}

// ProjectRunners lists the runners with an active session for the project (several may serve it,
// and one may serve several projects).
func (c *Client) ProjectRunners(ctx context.Context, slug string) ([]proto.ProjectRunner, error) {
	var out []proto.ProjectRunner
	return out, c.json(ctx, http.MethodGet, "/api/v1/projects/"+esc(slug)+"/runners", nil, &out)
}

// ProjectsFiltered lists projects; archived is "" or "active" (default), "archived" or "all".
func (c *Client) ProjectsFiltered(ctx context.Context, archived string) ([]proto.ProjectView, error) {
	var out []proto.ProjectView
	q := ""
	if archived != "" {
		q = "?archived=" + url.QueryEscape(archived)
	}
	return out, c.json(ctx, http.MethodGet, "/api/v1/projects"+q, nil, &out)
}

// ArchiveRefusedError is returned by ArchiveProject when runs are active on the project.
type ArchiveRefusedError struct {
	Message string
	Runs    []proto.ProjectArchiveRunView
}

func (e *ArchiveRefusedError) Error() string { return e.Message }

// ArchiveProject archives a project. Active runs make it fail with *ArchiveRefusedError.
func (c *Client) ArchiveProject(ctx context.Context, slug string) (proto.ProjectArchiveResult, error) {
	var out proto.ProjectArchiveResult
	err := c.json(ctx, http.MethodPost, "/api/v1/projects/"+esc(slug)+"/archive", nil, &out)
	var ce *Error
	if errors.As(err, &ce) && ce.Status == http.StatusConflict {
		var ref proto.ProjectArchiveRefusal
		if json.Unmarshal(ce.Body, &ref) == nil && len(ref.Runs) > 0 {
			return out, &ArchiveRefusedError{Message: ref.Error, Runs: ref.Runs}
		}
	}
	return out, err
}

// RestoreProject brings an archived project back.
func (c *Client) RestoreProject(ctx context.Context, slug string) (proto.ProjectView, error) {
	var out proto.ProjectView
	return out, c.json(ctx, http.MethodPost, "/api/v1/projects/"+esc(slug)+"/restore", nil, &out)
}
func (c *Client) Project(ctx context.Context, slug string) (proto.ProjectView, error) {
	var out proto.ProjectView
	return out, c.json(ctx, http.MethodGet, "/api/v1/projects/"+esc(slug), nil, &out)
}
func (c *Client) CreateProject(ctx context.Context, req proto.ProjectRequest) (proto.ProjectView, error) {
	var out proto.ProjectView
	return out, c.json(ctx, http.MethodPost, "/api/v1/projects", req, &out)
}
func (c *Client) UpdateProject(ctx context.Context, slug string, req proto.ProjectRequest) (proto.ProjectView, error) {
	var out proto.ProjectView
	return out, c.json(ctx, http.MethodPatch, "/api/v1/projects/"+esc(slug), req, &out)
}
func (c *Client) DeleteProject(ctx context.Context, slug string) error {
	return c.json(ctx, http.MethodDelete, "/api/v1/projects/"+esc(slug), nil, nil)
}

// --- repos & issues ---

func (c *Client) Repos(ctx context.Context, project string) ([]proto.RepoView, error) {
	var out []proto.RepoView
	return out, c.json(ctx, http.MethodGet, "/api/v1/repos?project="+url.QueryEscape(project), nil, &out)
}
func (c *Client) LinkRepo(ctx context.Context, project, repo, branch string) (proto.RepoView, error) {
	var out proto.RepoView
	return out, c.json(ctx, http.MethodPost, "/api/v1/projects/"+esc(project)+"/repos", proto.LinkRepoRequest{Repo: repo, Branch: branch}, &out)
}
func (c *Client) UnlinkRepo(ctx context.Context, project, repo string) error {
	owner, name, _ := strings.Cut(repo, "/")
	return c.json(ctx, http.MethodDelete, "/api/v1/projects/"+esc(project)+"/repos/"+esc(owner)+"/"+esc(name), nil, nil)
}
func (c *Client) SyncIssues(ctx context.Context, project string) (int, error) {
	var out struct {
		Synced int `json:"synced"`
	}
	return out.Synced, c.json(ctx, http.MethodPost, "/api/v1/projects/"+esc(project)+"/issues/sync", nil, &out)
}

// IssueDrift lists issues whose synced row differs from GitHub; repair re-syncs them.
func (c *Client) IssueDrift(ctx context.Context, project string, repair bool) ([]issuestore.Drift, error) {
	var out []issuestore.Drift
	q := ""
	if repair {
		q = "?repair=1"
	}
	return out, c.json(ctx, http.MethodGet, "/api/v1/projects/"+esc(project)+"/issues/drift"+q, nil, &out)
}
func (c *Client) Issues(ctx context.Context, project, state string) ([]proto.IssueView, error) {
	var out []proto.IssueView
	return out, c.json(ctx, http.MethodGet, fmt.Sprintf("/api/v1/issues?project=%s&state=%s", url.QueryEscape(project), url.QueryEscape(state)), nil, &out)
}

func issuePath(project, ref string) (string, error) {
	repo, num, ok := strings.Cut(ref, "#")
	owner, name, ok2 := strings.Cut(repo, "/")
	if !ok || !ok2 {
		return "", fmt.Errorf("issue %q: want owner/name#number", ref)
	}
	return fmt.Sprintf("/api/v1/projects/%s/issues/%s/%s/%s", esc(project), esc(owner), esc(name), esc(num)), nil
}
func (c *Client) Issue(ctx context.Context, project, ref string) (proto.IssueView, error) {
	var out proto.IssueView
	p, err := issuePath(project, ref)
	if err != nil {
		return out, err
	}
	return out, c.json(ctx, http.MethodGet, p, nil, &out)
}
func (c *Client) IssueToTask(ctx context.Context, project, ref, parent, template string) (proto.TaskRef, error) {
	var out proto.TaskRef
	p, err := issuePath(project, ref)
	if err != nil {
		return out, err
	}
	return out, c.json(ctx, http.MethodPost, p+"/task", map[string]string{"parent": parent, "template": template}, &out)
}

// --- tasks ---

func (c *Client) ProjectTree(ctx context.Context, project string, all bool) ([]proto.TaskNode, error) {
	var out []proto.TaskNode
	return out, c.json(ctx, http.MethodGet, "/api/v1/projects/"+esc(project)+"/tree"+allQ(all), nil, &out)
}
func (c *Client) TaskTree(ctx context.Context, sqid string, all bool) ([]proto.TaskNode, error) {
	var out []proto.TaskNode
	return out, c.json(ctx, http.MethodGet, "/api/v1/tasks/"+esc(sqid)+"/tree"+allQ(all), nil, &out)
}
func allQ(all bool) string {
	if all {
		return "?all=1"
	}
	return ""
}
func (c *Client) Tasks(ctx context.Context, project, state, parent string) ([]proto.TaskRef, error) {
	var out []proto.TaskRef
	q := url.Values{"project": {project}, "state": {state}, "parent": {parent}}
	return out, c.json(ctx, http.MethodGet, "/api/v1/tasks?"+q.Encode(), nil, &out)
}
func (c *Client) CreateTask(ctx context.Context, req proto.TaskCreateRequest) (proto.TaskRef, error) {
	var out proto.TaskRef
	return out, c.json(ctx, http.MethodPost, "/api/v1/tasks", req, &out)
}
func (c *Client) Task(ctx context.Context, sqid string) (proto.TaskDetail, error) {
	var out proto.TaskDetail
	return out, c.json(ctx, http.MethodGet, "/api/v1/tasks/"+esc(sqid), nil, &out)
}
func (c *Client) EditTask(ctx context.Context, sqid string, req proto.TaskPatchRequest) (proto.TaskRef, error) {
	var out proto.TaskRef
	return out, c.json(ctx, http.MethodPatch, "/api/v1/tasks/"+esc(sqid), req, &out)
}
func (c *Client) MoveTask(ctx context.Context, sqid string, req proto.TaskMoveRequest) (proto.TaskRef, error) {
	var out proto.TaskRef
	return out, c.json(ctx, http.MethodPost, "/api/v1/tasks/"+esc(sqid)+"/move", req, &out)
}
func (c *Client) NeedsAdd(ctx context.Context, sqid, pre string) error {
	return c.json(ctx, http.MethodPost, "/api/v1/tasks/"+esc(sqid)+"/needs/"+esc(pre), nil, nil)
}
func (c *Client) NeedsRm(ctx context.Context, sqid, pre string) error {
	return c.json(ctx, http.MethodDelete, "/api/v1/tasks/"+esc(sqid)+"/needs/"+esc(pre), nil, nil)
}
func (c *Client) SplitTask(ctx context.Context, sqid string, req proto.TaskSplitRequest) ([]proto.TaskRef, error) {
	var out []proto.TaskRef
	return out, c.json(ctx, http.MethodPost, "/api/v1/tasks/"+esc(sqid)+"/split", req, &out)
}
func (c *Client) QueueTask(ctx context.Context, sqid string) (int64, string, error) {
	return c.QueueTaskWith(ctx, sqid, "")
}

// QueueTaskWith queues with an agent definition override ("" = the project's default).
func (c *Client) QueueTaskWith(ctx context.Context, sqid, agent string) (int64, string, error) {
	var out struct {
		RunID  int64  `json:"run_id"`
		Branch string `json:"branch"`
	}
	return out.RunID, out.Branch, c.json(ctx, http.MethodPost, "/api/v1/tasks/"+esc(sqid)+"/queue", proto.QueueRequest{Agent: agent}, &out)
}

// DequeueTask removes a task's queued run from the queue; it errors (409) when there is none.
func (c *Client) DequeueTask(ctx context.Context, sqid string) error {
	return c.json(ctx, http.MethodPost, "/api/v1/tasks/"+esc(sqid)+"/dequeue", nil, nil)
}

// SetQueueOrder ranks the queued runs in the given order (run ids).
func (c *Client) SetQueueOrder(ctx context.Context, runs []int64) error {
	return c.json(ctx, http.MethodPost, "/api/v1/runs/queue-order", map[string]any{"runs": runs}, nil)
}

// TaskAction posts complete | reopen | cancel.
func (c *Client) TaskAction(ctx context.Context, sqid, op, message string) error {
	return c.json(ctx, http.MethodPost, "/api/v1/tasks/"+esc(sqid)+"/"+op, proto.MessageRequest{Message: message}, nil)
}
func (c *Client) DeleteTask(ctx context.Context, sqid string) error {
	return c.json(ctx, http.MethodDelete, "/api/v1/tasks/"+esc(sqid), nil, nil)
}

// --- runs & logs ---

func (c *Client) Runs(ctx context.Context, state, runner, project string, limit int) ([]proto.RunView, error) {
	var out []proto.RunView
	q := url.Values{"state": {state}, "runner": {runner}, "project": {project}, "limit": {strconv.Itoa(limit)}}
	return out, c.json(ctx, http.MethodGet, "/api/v1/runs?"+q.Encode(), nil, &out)
}
func (c *Client) Run(ctx context.Context, id int64) (proto.RunView, error) {
	var out proto.RunView
	return out, c.json(ctx, http.MethodGet, fmt.Sprintf("/api/v1/runs/%d", id), nil, &out)
}
func (c *Client) CancelRun(ctx context.Context, id int64) error {
	return c.json(ctx, http.MethodPost, fmt.Sprintf("/api/v1/runs/%d/cancel", id), nil, nil)
}

// Inspect fetches diff | files | file for a run.
func (c *Client) Inspect(ctx context.Context, id int64, what, path string, stat bool) (proto.InspectReply, error) {
	var out proto.InspectReply
	q := url.Values{"path": {path}}
	if stat {
		q.Set("stat", "1")
	}
	return out, c.json(ctx, http.MethodGet, fmt.Sprintf("/api/v1/runs/%d/%s?%s", id, what, q.Encode()), nil, &out)
}

// streamReq opens a long-lived GET (no client timeout).
func (c *Client) streamReq(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	if c.cf != nil {
		req.Header.Set("CF-Access-Client-Id", c.cf.ID)
		req.Header.Set("CF-Access-Client-Secret", c.cf.Secret)
	}
	hc := *c.hc
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("controller unreachable: %w", err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var e proto.APIError
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return nil, &Error{Status: resp.StatusCode, Message: e.Error}
	}
	return resp, nil
}

// AttachPTY copies the run's terminal (replay, then live) to w until the run ends.
func (c *Client) AttachPTY(ctx context.Context, id int64, w io.Writer) error {
	resp, err := c.streamReq(ctx, fmt.Sprintf("/api/v1/runs/%d/pty", id))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// Logs streams a run's log lines to fn.
func (c *Client) Logs(ctx context.Context, id int64, follow bool, grep, stream, since string, fn func(proto.LogView)) error {
	q := url.Values{"grep": {grep}, "stream": {stream}, "since": {since}}
	if follow {
		q.Set("follow", "1")
	}
	resp, err := c.streamReq(ctx, fmt.Sprintf("/api/v1/runs/%d/logs?%s", id, q.Encode()))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var l proto.LogView
		if json.Unmarshal(sc.Bytes(), &l) == nil {
			fn(l)
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return sc.Err()
}

// SearchLogs runs a full-text search.
func (c *Client) SearchLogs(ctx context.Context, query, project, runner, since string, limit int) ([]proto.LogView, error) {
	var out []proto.LogView
	q := url.Values{"q": {query}, "project": {project}, "runner": {runner}, "since": {since}, "limit": {strconv.Itoa(limit)}}
	return out, c.json(ctx, http.MethodGet, "/api/v1/logs/search?"+q.Encode(), nil, &out)
}

// Questions lists questions (open only unless all).
func (c *Client) Questions(ctx context.Context, all bool) ([]proto.QuestionView, error) {
	var out []proto.QuestionView
	return out, c.json(ctx, http.MethodGet, "/api/v1/questions"+allQ(all), nil, &out)
}

// Question returns one question.
func (c *Client) Question(ctx context.Context, id int64) (proto.QuestionView, error) {
	var out proto.QuestionView
	return out, c.json(ctx, http.MethodGet, fmt.Sprintf("/api/v1/questions/%d", id), nil, &out)
}

// Answer answers a question with an option value and/or free text.
func (c *Client) Answer(ctx context.Context, id int64, value, text string) error {
	return c.json(ctx, http.MethodPost, fmt.Sprintf("/api/v1/questions/%d/answer", id), proto.AnswerRequest{Value: value, Text: text}, nil)
}

// RunHooks lists a run's agent hook events.
func (c *Client) RunHooks(ctx context.Context, id int64) ([]map[string]any, error) {
	var out []map[string]any
	return out, c.json(ctx, http.MethodGet, fmt.Sprintf("/api/v1/runs/%d/hooks", id), nil, &out)
}

// PRs lists pull requests (state: open | merged | closed | all).
func (c *Client) PRs(ctx context.Context, state string) ([]proto.PRView, error) {
	var out []proto.PRView
	return out, c.json(ctx, http.MethodGet, "/api/v1/prs?state="+url.QueryEscape(state), nil, &out)
}

// PR returns one pull request.
func (c *Client) PR(ctx context.Context, id int64) (proto.PRView, error) {
	var out proto.PRView
	return out, c.json(ctx, http.MethodGet, fmt.Sprintf("/api/v1/prs/%d", id), nil, &out)
}

// PRAction posts approve | request-changes | resolve | close.
func (c *Client) PRAction(ctx context.Context, id int64, op, message string) error {
	return c.json(ctx, http.MethodPost, fmt.Sprintf("/api/v1/prs/%d/%s", id, op), proto.MessageRequest{Message: message}, nil)
}

// Event is a change notice from GET /api/v1/events.
type Event struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Events streams change notices to fn until ctx ends or the stream breaks.
func (c *Client) Events(ctx context.Context, fn func(Event)) error {
	resp, err := c.streamReq(ctx, "/api/v1/events")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var e Event
			if json.Unmarshal([]byte(data), &e) == nil {
				fn(e)
			}
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return sc.Err()
}

// PlanTask queues a plan run ("split with agent").
func (c *Client) PlanTask(ctx context.Context, sqid, guidance string) (int64, error) {
	var out struct {
		RunID int64 `json:"run_id"`
	}
	return out.RunID, c.json(ctx, http.MethodPost, "/api/v1/tasks/"+esc(sqid)+"/plan", map[string]string{"guidance": guidance}, &out)
}

// PushTest sends a test notification to every subscribed browser.
func (c *Client) PushTest(ctx context.Context) (int, error) {
	var out struct {
		Delivered int `json:"delivered"`
	}
	return out.Delivered, c.json(ctx, http.MethodPost, "/api/v1/push/test", nil, &out)
}

// ThreadMessage is one message of a task's conversation with a controller agent.
type ThreadMessage struct {
	ID     int64  `json:"id"`
	At     string `json:"at"`
	Role   string `json:"role"`
	Body   string `json:"body"`
	Status string `json:"status"`
	// Proposals is a JSON array of {label, description, action, state, result}.
	Proposals string `json:"proposals"`
}

// ApplyProposal approves proposal n (0-based) of a thread message.
func (c *Client) ApplyProposal(ctx context.Context, msgID int64, n int) error {
	return c.json(ctx, http.MethodPost, fmt.Sprintf("/api/v1/thread/%d/apply/%d", msgID, n), nil, nil)
}

// AskTask asks a controller agent (agent "" = the default) about a task and its subtree.
func (c *Client) AskTask(ctx context.Context, sqid, agent, question string) error {
	return c.json(ctx, http.MethodPost, "/api/v1/tasks/"+esc(sqid)+"/ask", map[string]string{"agent": agent, "question": question}, nil)
}

// TaskThread lists a task's conversation with controller agents.
func (c *Client) TaskThread(ctx context.Context, sqid string) ([]ThreadMessage, error) {
	var out []ThreadMessage
	return out, c.json(ctx, http.MethodGet, "/api/v1/tasks/"+esc(sqid)+"/thread", nil, &out)
}

// --- agent definitions ---

func (c *Client) AgentDefs(ctx context.Context) ([]proto.AgentView, error) {
	var out []proto.AgentView
	return out, c.json(ctx, http.MethodGet, "/api/v1/agent-defs", nil, &out)
}
func (c *Client) AgentDef(ctx context.Context, name string) (proto.AgentView, error) {
	var out proto.AgentView
	return out, c.json(ctx, http.MethodGet, "/api/v1/agent-defs/"+esc(name), nil, &out)
}
func (c *Client) CreateAgentDef(ctx context.Context, req proto.AgentRequest) (proto.AgentView, error) {
	var out proto.AgentView
	return out, c.json(ctx, http.MethodPost, "/api/v1/agent-defs", req, &out)
}
func (c *Client) UpdateAgentDef(ctx context.Context, name string, req proto.AgentRequest) (proto.AgentView, error) {
	var out proto.AgentView
	return out, c.json(ctx, http.MethodPut, "/api/v1/agent-defs/"+esc(name), req, &out)
}
func (c *Client) DeleteAgentDef(ctx context.Context, name string) error {
	return c.json(ctx, http.MethodDelete, "/api/v1/agent-defs/"+esc(name), nil, nil)
}

// AllowAgent allows (or with allow=false, stops allowing) an agent on a template.
func (c *Client) AllowAgent(ctx context.Context, agent, template string, allow bool) error {
	m := http.MethodPut
	if !allow {
		m = http.MethodDelete
	}
	return c.json(ctx, m, "/api/v1/agent-defs/"+esc(agent)+"/templates/"+esc(template), nil, nil)
}

// --- secrets (names only; values are write-only) ---

func (c *Client) Secrets(ctx context.Context) ([]string, error) {
	var out []string
	return out, c.json(ctx, http.MethodGet, "/api/v1/secrets", nil, &out)
}

// SetSecret stores a secret on the controller.
func (c *Client) SetSecret(ctx context.Context, name, value string) error {
	return c.do(ctx, http.MethodPut, "/api/v1/secrets/"+esc(name), strings.NewReader(value), "text/plain", nil)
}

// --- remote targets ---

func (c *Client) Targets(ctx context.Context, all bool) ([]proto.TargetView, error) {
	var out []proto.TargetView
	q := ""
	if all {
		q = "?all=1"
	}
	return out, c.json(ctx, http.MethodGet, "/api/v1/targets"+q, nil, &out)
}
func (c *Client) TerminateTarget(ctx context.Context, name string) error {
	return c.json(ctx, http.MethodDelete, "/api/v1/targets/"+esc(name), nil, nil)
}

// AskProject asks a controller agent about a whole project (the project chat; "/queue x" and
// "/dequeue x" are commands answered without the model).
func (c *Client) AskProject(ctx context.Context, slug, agent, question string) error {
	return c.json(ctx, http.MethodPost, "/api/v1/projects/"+esc(slug)+"/ask", map[string]string{"agent": agent, "question": question}, nil)
}

// ProjectThread is the project chat as JSON: {"messages": [...], "context": {...}}.
func (c *Client) ProjectThread(ctx context.Context, slug string) (json.RawMessage, error) {
	var out json.RawMessage
	return out, c.json(ctx, http.MethodGet, "/api/v1/projects/"+esc(slug)+"/thread", nil, &out)
}

// ApplyProjectProposal approves option n of a project chat message.
func (c *Client) ApplyProjectProposal(ctx context.Context, msgID int64, n int) error {
	return c.json(ctx, http.MethodPost, fmt.Sprintf("/api/v1/project-thread/%d/apply/%d", msgID, n), nil, nil)
}

// --- custom tool library (docs/wrangl/adr/0003) ---

func (c *Client) Tools(ctx context.Context) ([]proto.LibraryToolView, error) {
	var out []proto.LibraryToolView
	return out, c.json(ctx, http.MethodGet, "/api/v1/tools", nil, &out)
}
func (c *Client) ProjectTools(ctx context.Context, project string) ([]proto.LibraryToolView, error) {
	var out []proto.LibraryToolView
	return out, c.json(ctx, http.MethodGet, "/api/v1/projects/"+esc(project)+"/tools", nil, &out)
}
func (c *Client) Tool(ctx context.Context, name string) (proto.LibraryToolView, error) {
	var out proto.LibraryToolView
	return out, c.json(ctx, http.MethodGet, "/api/v1/tools/"+esc(name), nil, &out)
}
func (c *Client) AddTool(ctx context.Context, kdl string) (proto.LibraryToolView, error) {
	var out proto.LibraryToolView
	return out, c.json(ctx, http.MethodPost, "/api/v1/tools", proto.LibraryToolRequest{KDL: kdl}, &out)
}
func (c *Client) UpdateTool(ctx context.Context, name, kdl string) (proto.LibraryToolView, error) {
	var out proto.LibraryToolView
	return out, c.json(ctx, http.MethodPut, "/api/v1/tools/"+esc(name), proto.LibraryToolRequest{KDL: kdl}, &out)
}
func (c *Client) RemoveTool(ctx context.Context, name string, force bool) error {
	p := "/api/v1/tools/" + esc(name)
	if force {
		p += "?force=1"
	}
	return c.json(ctx, http.MethodDelete, p, nil, nil)
}

// EnableTool enables (or with enable=false, disables) a library tool for a project.
func (c *Client) EnableTool(ctx context.Context, project, name string, enable bool) error {
	m := http.MethodPut
	if !enable {
		m = http.MethodDelete
	}
	return c.json(ctx, m, "/api/v1/projects/"+esc(project)+"/tools/"+esc(name), nil, nil)
}

// TestTool calls a tool enabled on a project with sample input.
func (c *Client) TestTool(ctx context.Context, project, name string, input json.RawMessage) error {
	return c.json(ctx, http.MethodPost, "/api/v1/projects/"+esc(project)+"/tools/"+esc(name)+"/test", proto.LibraryToolTestRequest{Input: input}, nil)
}

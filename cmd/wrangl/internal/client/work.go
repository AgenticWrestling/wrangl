package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/nreid/devhost/cmd/wrangl/internal/proto"
)

func esc(s string) string { return url.PathEscape(s) }

// --- projects ---

func (c *Client) Projects(ctx context.Context) ([]proto.ProjectView, error) {
	var out []proto.ProjectView
	return out, c.json(ctx, http.MethodGet, "/api/v1/projects", nil, &out)
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
	var out struct {
		RunID  int64  `json:"run_id"`
		Branch string `json:"branch"`
	}
	return out.RunID, out.Branch, c.json(ctx, http.MethodPost, "/api/v1/tasks/"+esc(sqid)+"/queue", nil, &out)
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

// PRAction posts approve | request-changes | close.
func (c *Client) PRAction(ctx context.Context, id int64, op, message string) error {
	return c.json(ctx, http.MethodPost, fmt.Sprintf("/api/v1/prs/%d/%s", id, op), proto.MessageRequest{Message: message}, nil)
}

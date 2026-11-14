// Package apitools exposes the controller's REST API to the owner's remote MCP endpoint as
// tools. Each tool is one row of a table that names a REST route; a call is dispatched to
// the very same handler in-process, so the rules, validation and side effects are exactly
// those of the CLI and web UI. The /mcp endpoint already sits behind the same access
// control as /api/v1, so no further authentication happens here.
package apitools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/mcp"
)

// Route is one tool.
type Route struct {
	Name, Desc string
	Method     string // GET, POST, PUT, PATCH, DELETE
	Path       string // under /api/v1, with {param} placeholders filled from the arguments
	Props      string // JSON Schema properties, without the surrounding braces
	Required   []string
	Query      []string // arguments sent in the query string (default for GET and DELETE: all non-path arguments)
	RawBody    string   // argument whose string value is the whole request body (templates are YAML)
	Confirm    string   // destructive: the argument "confirm" must equal this argument
	Tail       bool     // a long text result: keep the end when over the size cap
	Pre        func(args map[string]any) error
}

const maxResult = 200 << 10

// Handler is the controller's HTTP handler (the mux); set before the first call.
type Service struct{ Handler http.Handler }

func p(name, typ, desc string) string {
	d, _ := json.Marshal(desc)
	return fmt.Sprintf(`%q:{"type":%q,"description":%s}`, name, typ, d)
}

func props(parts ...string) string { return strings.Join(parts, ",") }

var (
	idArg    = p("id", "integer", "numeric id")
	sqidArg  = p("sqid", "string", "task sqid")
	slugArg  = p("slug", "string", "project slug")
	nameArg  = p("name", "string", "name")
	confirm  = p("confirm", "string", "irreversible: repeat the name to proceed")
	refArg   = p("ref", "string", "issue reference owner/name#number")
	prIDArg  = p("id", "integer", "pull request id (from pr_list)")
	tmplArg  = p("template", "string", "runner template name")
	allFlag  = p("all", "boolean", "include finished or archived rows")
	queryArg = p("q", "string", "search words")
)

// splitRepo turns repo "owner/name" into the owner and name path parameters.
func splitRepo(args map[string]any) error {
	r, _ := args["repo"].(string)
	o, n, ok := strings.Cut(r, "/")
	if !ok || o == "" || n == "" {
		return fmt.Errorf("%w: repo must be owner/name", errs.Invalid)
	}
	args["owner"], args["name"] = o, n
	delete(args, "repo")
	return nil
}

var refRe = regexp.MustCompile(`^([^/\s]+)/([^#\s]+)#(\d+)$`)

// splitRef turns ref "owner/name#n" into owner, name and number path parameters.
func splitRef(args map[string]any) error {
	r, _ := args["ref"].(string)
	m := refRe.FindStringSubmatch(r)
	if m == nil {
		return fmt.Errorf("%w: ref must look like owner/name#123", errs.Invalid)
	}
	args["owner"], args["name"], args["number"] = m[1], m[2], m[3]
	delete(args, "ref")
	return nil
}

// Routes is the catalogue. Deliberately absent (see docs/wrangl/guide): anything that mints
// or reads credentials (enroll tokens, secret values, push subscriptions, GitHub App setup),
// interactive terminals and shells on runners, adopting or terminating raw EC2 instances,
// and the web UI's own streams.
var Routes = []Route{
	// --- the Inbox ---
	{Name: "question_list", Desc: "List open questions: agent questions, approvals, task and PR reviews, plan reviews. all=true includes answered ones.", Method: "GET", Path: "/questions", Props: allFlag},
	{Name: "question_get", Desc: "Show one question with its options and detail.", Method: "GET", Path: "/questions/{id}", Props: idArg, Required: []string{"id"}},
	{Name: "question_answer", Desc: "Answer a question with one of its option values and/or free text. This is how you unblock a waiting run or review.", Method: "POST", Path: "/questions/{id}/answer",
		Props: props(idArg, p("value", "string", "an option value from question_get"), p("text", "string", "free-text answer or extra detail")), Required: []string{"id"}},

	// --- runs ---
	{Name: "run_get", Desc: "Show one run: state, task, runner, branch, timings, error.", Method: "GET", Path: "/runs/{id}", Props: idArg, Required: []string{"id"}},
	{Name: "run_cancel", Desc: "Cancel a run.", Method: "POST", Path: "/runs/{id}/cancel", Props: idArg, Required: []string{"id"}},
	{Name: "run_logs", Desc: "A run's log lines (NDJSON), optionally from the last duration (since, e.g. 15m) and filtered by a regexp (grep). Large output keeps the end.", Method: "GET", Path: "/runs/{id}/logs", Tail: true,
		Props: props(idArg, p("since", "string", "duration like 15m, or an RFC 3339 time"), p("grep", "string", "regexp on the message"), p("stream", "string", "log stream name")), Required: []string{"id"}},
	{Name: "run_transcript", Desc: "The agent transcript of a run (latest entries; page back with before).", Method: "GET", Path: "/runs/{id}/transcript", Tail: true,
		Props: props(idArg, p("limit", "integer", "entries, default 50"), p("before", "integer", "page cursor")), Required: []string{"id"}},
	{Name: "run_queue_order", Desc: "Set the order of queued runs: pass run ids, first = next to start.", Method: "POST", Path: "/runs/queue-order",
		Props: `"runs":{"type":"array","items":{"type":"integer"},"description":"run ids in the order they should start"}`, Required: []string{"runs"}},
	{Name: "log_search", Desc: "Full-text search over run logs (FTS5 syntax), optionally within a project or runner.", Method: "GET", Path: "/logs/search",
		Props: props(queryArg, p("project", "string", "project slug"), p("runner", "string", "runner name"), p("since", "string", "duration like 1h"), p("limit", "integer", "default 100")), Required: []string{"q"}},

	// --- tasks beyond create/update/queue ---
	{Name: "task_dequeue", Desc: "Take a task's queued run off the queue.", Method: "POST", Path: "/tasks/{sqid}/dequeue", Props: sqidArg, Required: []string{"sqid"}},
	{Name: "task_plan", Desc: "Queue a plan run that proposes how to split a task into subtasks.", Method: "POST", Path: "/tasks/{sqid}/plan", Props: sqidArg, Required: []string{"sqid"}},
	{Name: "task_split", Desc: "Split a task into subtasks yourself: give children (nested {title, prompt, type, children}) or an indented outline. chain=true makes each need the previous.", Method: "POST", Path: "/tasks/{sqid}/split",
		Props: props(sqidArg, `"children":{"type":"array","items":{"type":"object"}}`, p("outline", "string", "indented outline to parse"), p("chain", "boolean", ""), p("copy_prompt", "boolean", "give each child the parent's prompt")), Required: []string{"sqid"}},
	{Name: "task_ask", Desc: "Ask a read-only agent a question about a task; the answer arrives in the task thread (task_thread).", Method: "POST", Path: "/tasks/{sqid}/ask",
		Props: props(sqidArg, p("agent", "string", "agent name"), p("question", "string", "")), Required: []string{"sqid", "question"}},
	{Name: "task_thread", Desc: "The task's discussion thread with agents, including proposals.", Method: "GET", Path: "/tasks/{sqid}/thread", Props: sqidArg, Required: []string{"sqid"}},
	{Name: "thread_apply", Desc: "Apply proposal n of a task-thread message (a suggested change to the task tree).", Method: "POST", Path: "/thread/{id}/apply/{n}",
		Props: props(idArg, p("n", "integer", "proposal index")), Required: []string{"id", "n"}},
	{Name: "project_tree", Desc: "The ranked task tree of a project (all=true includes done and cancelled).", Method: "GET", Path: "/projects/{slug}/tree", Props: props(slugArg, allFlag), Required: []string{"slug"}},

	// --- projects: chat, runners, repos, issues ---
	{Name: "project_ask", Desc: "Ask the project chat agent about the whole project; read the reply with project_thread.", Method: "POST", Path: "/projects/{slug}/ask",
		Props: props(slugArg, p("agent", "string", "agent name"), p("question", "string", "")), Required: []string{"slug", "question"}},
	{Name: "project_thread", Desc: "The project chat thread, including proposals.", Method: "GET", Path: "/projects/{slug}/thread", Props: slugArg, Required: []string{"slug"}},
	{Name: "project_thread_apply", Desc: "Apply proposal n of a project-chat message.", Method: "POST", Path: "/project-thread/{id}/apply/{n}",
		Props: props(idArg, p("n", "integer", "proposal index")), Required: []string{"id", "n"}},
	{Name: "project_runners", Desc: "The runners a project may use and its per-template instance counts.", Method: "GET", Path: "/projects/{slug}/runners", Props: slugArg, Required: []string{"slug"}},
	{Name: "repo_list", Desc: "List linked GitHub repos, optionally for one project.", Method: "GET", Path: "/repos", Props: p("project", "string", "project slug")},
	{Name: "repo_link", Desc: "Link a GitHub repo (owner/name) to a project. The wrangl GitHub App must be installed on it.", Method: "POST", Path: "/projects/{slug}/repos",
		Props: props(slugArg, p("repo", "string", "owner/name"), p("branch", "string", "default branch override")), Required: []string{"slug", "repo"}},
	{Name: "repo_edit", Desc: "Set the one-line note on a linked repo (what it is, shown to agents by repo_list); an empty note clears it.", Method: "PATCH", Path: "/projects/{slug}/repos/{owner}/{name}", Pre: splitRepo,
		Props: props(slugArg, p("repo", "string", "owner/name"), p("note", "string", "one line")), Required: []string{"slug", "repo"}},
	{Name: "repo_unlink", Desc: "Unlink a repo from a project.", Method: "DELETE", Path: "/projects/{slug}/repos/{owner}/{name}", Pre: splitRepo,
		Props: props(slugArg, p("repo", "string", "owner/name")), Required: []string{"slug", "repo"}},
	{Name: "issue_list", Desc: "List synced GitHub issues, optionally for one project (state open by default; closed, all).", Method: "GET", Path: "/issues",
		Props: props(p("project", "string", "project slug"), p("state", "string", "open, closed or all"))},
	{Name: "issue_sync", Desc: "Sync a project's GitHub issues now. Returns counts per repo.", Method: "POST", Path: "/projects/{slug}/issues/sync", Props: slugArg, Required: []string{"slug"}},
	{Name: "issue_drift", Desc: "Show tasks whose linked issue state disagrees with the task state; repair=true fixes them.", Method: "GET", Path: "/projects/{slug}/issues/drift",
		Props: props(slugArg, p("repair", "string", "set to 1 to repair")), Required: []string{"slug"}},
	{Name: "issue_to_task", Desc: "Create a task from a synced GitHub issue.", Method: "POST", Path: "/projects/{slug}/issues/{owner}/{name}/{number}/task", Pre: splitRef,
		Props: props(slugArg, refArg, p("parent", "string", "parent task sqid"), tmplArg), Required: []string{"slug", "ref"}},

	// --- pull requests ---
	{Name: "pr_list", Desc: "List pull requests (state open by default; merged, closed, all).", Method: "GET", Path: "/prs", Props: p("state", "string", "open, merged, closed or all")},
	{Name: "pr_get", Desc: "Show one pull request with its review, risk and gate state.", Method: "GET", Path: "/prs/{id}", Props: prIDArg, Required: []string{"id"}},
	{Name: "pr_approve", Desc: "Approve a pull request (merges it when every gate passes).", Method: "POST", Path: "/prs/{id}/approve", Props: prIDArg, Required: []string{"id"}},
	{Name: "pr_request_changes", Desc: "Send a pull request back to the agent with a message.", Method: "POST", Path: "/prs/{id}/request-changes",
		Props: props(prIDArg, p("message", "string", "what to change")), Required: []string{"id", "message"}},
	{Name: "pr_resolve", Desc: "Queue Smart Merge: an agent merges the base branch in and resolves conflicts.", Method: "POST", Path: "/prs/{id}/resolve", Props: prIDArg, Required: []string{"id"}},
	{Name: "pr_close", Desc: "Close a pull request without merging.", Method: "POST", Path: "/prs/{id}/close", Props: prIDArg, Required: []string{"id"}},

	// --- fleet ---
	{Name: "runner_list", Desc: "List runners (all=true includes offline and terminated).", Method: "GET", Path: "/runners", Props: allFlag},
	{Name: "runner_get", Desc: "Show one runner: state, template and version, active runs, health.", Method: "GET", Path: "/runners/{name}", Props: nameArg, Required: []string{"name"}},
	{Name: "runner_launch", Desc: "Launch runner instances from a template (count 1..20). Costs money.", Method: "POST", Path: "/runners",
		Props: props(tmplArg, p("count", "integer", "default 1")), Required: []string{"template"}},
	{Name: "runner_cordon", Desc: "Stop a runner taking new runs.", Method: "POST", Path: "/runners/{name}/cordon", Props: nameArg, Required: []string{"name"}},
	{Name: "runner_uncordon", Desc: "Let a cordoned runner take runs again.", Method: "POST", Path: "/runners/{name}/uncordon", Props: nameArg, Required: []string{"name"}},
	{Name: "runner_drain", Desc: "Cordon a runner and retire it once its runs finish.", Method: "POST", Path: "/runners/{name}/drain", Props: nameArg, Required: []string{"name"}},
	{Name: "runner_delete", Desc: "Remove a runner (terminates a launched instance). Pass confirm equal to the name.", Method: "DELETE", Path: "/runners/{name}", Confirm: "name", Props: props(nameArg, confirm), Required: []string{"name", "confirm"}},
	{Name: "template_list", Desc: "List runner templates.", Method: "GET", Path: "/templates"},
	{Name: "template_get", Desc: "Show a runner template: size, AMI, mise config, setup script, agent, limits, version.", Method: "GET", Path: "/templates/{name}", Props: nameArg, Required: []string{"name"}},
	{Name: "template_create", Desc: "Create a runner template from its YAML spec (see docs/wrangl/templates/*.yaml).", Method: "POST", Path: "/templates", RawBody: "spec",
		Props: p("spec", "string", "the template YAML"), Required: []string{"spec"}},
	{Name: "template_update", Desc: "Replace a runner template with a new YAML spec (bumps its version; idle runners upgrade).", Method: "PUT", Path: "/templates/{name}", RawBody: "spec",
		Props: props(nameArg, p("spec", "string", "the template YAML")), Required: []string{"name", "spec"}},
	{Name: "template_delete", Desc: "Delete a runner template. Pass confirm equal to the name.", Method: "DELETE", Path: "/templates/{name}", Confirm: "name", Props: props(nameArg, confirm), Required: []string{"name", "confirm"}},
	{Name: "target_list", Desc: "List remote targets (Mac and Android emulator hosts driven over SSH/ADB).", Method: "GET", Path: "/targets", Props: allFlag},
	{Name: "costs_get", Desc: "Current runner spend summary and any cost lookup errors.", Method: "GET", Path: "/costs"},

	// --- agents ---
	{Name: "agent_def_list", Desc: "List named agent definitions (claude, codex, ...) and the templates that allow them.", Method: "GET", Path: "/agent-defs"},
	{Name: "agent_def_get", Desc: "Show one agent definition.", Method: "GET", Path: "/agent-defs/{name}", Props: nameArg, Required: []string{"name"}},
	{Name: "agent_def_create", Desc: "Create an agent definition from a JSON spec.", Method: "POST", Path: "/agent-defs",
		Props: props(nameArg, p("description", "string", ""), `"spec":{"type":"object","description":"the agent spec"}`), Required: []string{"name", "spec"}},
	{Name: "agent_def_update", Desc: "Replace an agent definition's description and spec.", Method: "PUT", Path: "/agent-defs/{name}",
		Props: props(nameArg, p("description", "string", ""), `"spec":{"type":"object","description":"the agent spec"}`), Required: []string{"name", "spec"}},
	{Name: "agent_def_delete", Desc: "Delete an agent definition. Pass confirm equal to the name.", Method: "DELETE", Path: "/agent-defs/{name}", Confirm: "name", Props: props(nameArg, confirm), Required: []string{"name", "confirm"}},
	{Name: "agent_def_allow", Desc: "Allow an agent definition on a runner template.", Method: "PUT", Path: "/agent-defs/{name}/templates/{template}", Props: props(nameArg, tmplArg), Required: []string{"name", "template"}},
	{Name: "agent_def_disallow", Desc: "Stop allowing an agent definition on a runner template.", Method: "DELETE", Path: "/agent-defs/{name}/templates/{template}", Props: props(nameArg, tmplArg), Required: []string{"name", "template"}},
	{Name: "agent_list", Desc: "List the controller agents (the project and task chat agents) and which is the default.", Method: "GET", Path: "/agents"},
	{Name: "secret_list", Desc: "List the names of controller secrets. Values are write-only and never returned; set them in Settings or with the CLI.", Method: "GET", Path: "/secrets"},
}

// Tools returns the MCP definitions.
func Tools() []mcp.Tool {
	out := make([]mcp.Tool, 0, len(Routes))
	for _, r := range Routes {
		req, _ := json.Marshal(append([]string{}, r.Required...))
		if len(r.Required) == 0 {
			req = []byte("[]")
		}
		schema := fmt.Sprintf(`{"type":"object","properties":{%s},"required":%s}`, r.Props, req)
		out = append(out, mcp.Tool{Name: r.Name, Description: r.Desc, InputSchema: json.RawMessage(schema)})
	}
	return out
}

// Has reports whether name is one of these tools.
func Has(name string) bool {
	for _, r := range Routes {
		if r.Name == name {
			return true
		}
	}
	return false
}

var placeholder = regexp.MustCompile(`\{([a-z_]+)\}`)

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprint(x)
	case bool:
		if x {
			return "1"
		}
		return ""
	default:
		return fmt.Sprint(x)
	}
}

// Call runs a tool by dispatching to the REST handler.
func (s *Service) Call(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	var rt *Route
	for i := range Routes {
		if Routes[i].Name == name {
			rt = &Routes[i]
		}
	}
	if rt == nil {
		return nil, fmt.Errorf("%w: unknown tool %s", errs.Invalid, name)
	}
	args := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("%w: arguments: %v", errs.Invalid, err)
		}
	}
	for _, k := range rt.Required {
		if v, ok := args[k]; !ok || str(v) == "" && fmt.Sprint(v) != "0" {
			return nil, fmt.Errorf("%w: %s is required", errs.Invalid, k)
		}
	}
	if rt.Confirm != "" {
		if str(args["confirm"]) != str(args[rt.Confirm]) {
			return nil, fmt.Errorf("%w: %s is irreversible; pass confirm=%q to proceed", errs.Invalid, rt.Name, str(args[rt.Confirm]))
		}
		delete(args, "confirm")
	}
	if rt.Pre != nil {
		if err := rt.Pre(args); err != nil {
			return nil, err
		}
	}
	path := placeholder.ReplaceAllStringFunc(rt.Path, func(m string) string {
		k := m[1 : len(m)-1]
		v := str(args[k])
		delete(args, k)
		return url.PathEscape(v)
	})
	var body io.Reader
	q := url.Values{}
	switch {
	case rt.RawBody != "":
		body = strings.NewReader(str(args[rt.RawBody]))
	case rt.Method == http.MethodGet || rt.Method == http.MethodDelete:
		for k, v := range args {
			q.Set(k, str(v))
		}
	default:
		for _, k := range rt.Query {
			if v, ok := args[k]; ok {
				q.Set(k, str(v))
				delete(args, k)
			}
		}
		b, _ := json.Marshal(args)
		body = bytes.NewReader(b)
	}
	target := "/api/v1" + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req := httptest.NewRequest(rt.Method, target, body).WithContext(ctx)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.Handler.ServeHTTP(rec, req)
	out := rec.Body.Bytes()
	if rec.Code >= 300 {
		msg := strings.TrimSpace(string(out))
		var e struct{ Error string }
		if json.Unmarshal(out, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, fmt.Errorf("%s (HTTP %d)", msg, rec.Code)
	}
	if len(out) == 0 {
		return map[string]bool{"ok": true}, nil
	}
	if len(out) > maxResult {
		note := fmt.Sprintf("[truncated: %d bytes, showing the %s %d]\n", len(out), map[bool]string{true: "last", false: "first"}[rt.Tail], maxResult)
		if rt.Tail {
			out = append([]byte(note), out[len(out)-maxResult:]...)
		} else {
			out = append(out[:maxResult:maxResult], []byte("\n"+note)...)
		}
		return string(out), nil
	}
	if json.Valid(out) {
		return json.RawMessage(out), nil
	}
	return string(out), nil // NDJSON and other text
}

// Names lists the tool names, sorted (for tests and docs).
func Names() []string {
	var n []string
	for _, r := range Routes {
		n = append(n, r.Name)
	}
	sort.Strings(n)
	return n
}

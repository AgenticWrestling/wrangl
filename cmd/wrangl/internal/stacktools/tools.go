// Package stacktools defines the MCP tools agents use on the work stack (spec §8). The
// runner's MCP shim and the controller's remote MCP endpoint both serve this catalogue;
// the controller enforces scope.
package stacktools

import (
	"encoding/json"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/mcp"
)

var position = `"position": {"type": "object", "description": "where among siblings (default last)", "properties": {"first": {"type": "boolean"}, "after": {"type": "string", "description": "sibling sqid"}, "before": {"type": "string", "description": "sibling sqid"}}}`

var taskType = `"target_date":{"type":"string","description":"target date, YYYY-MM-DD (empty clears it)"},"type":{"type":"string","enum":["code","bug","ux","architecture","docs","research","review","release","milestone","feature"],"description":"task type (default: the parent's, else code)"},`

func raw(s string) json.RawMessage { return json.RawMessage(s) }

// All is every tool. Which ones a caller may use depends on its run kind (Allowed).
var All = []mcp.Tool{
	{Name: "task_get", Description: "Get a task: title, prompt, state, parent, subtasks and precursors (with whether each is satisfied).",
		InputSchema: mcp.Schema(map[string]string{"sqid": "string"}, "sqid")},
	{Name: "task_tree", Description: "The ranked subtree under a task (default: your assignment root). Each node has sqid, title, state, depth and unmet precursor count.",
		InputSchema: mcp.Schema(map[string]string{"sqid": "string"})},
	{Name: "task_list", Description: "List tasks in the project, optionally filtered by state (open, in_progress, in_review, done, cancelled) and by text in the title.",
		InputSchema: mcp.Schema(map[string]string{"state": "string", "text": "string"})},
	{Name: "task_next", Description: "Peek at the next ready task in your assignment (does not change anything).",
		InputSchema: mcp.Schema(nil)},
	{Name: "task_create", Description: "Create a subtask under a task in your assignment. Use this when you discover more work.",
		InputSchema: raw(`{"type":"object","required":["parent","title"],"properties":{"parent":{"type":"string"},"title":{"type":"string"},"prompt":{"type":"string"},"type":{"type":"string","enum":["code","bug","ux","architecture","docs","research","review","release","milestone","feature"],"description":"task type (default: the parent's, else code)"},"needs":{"type":"array","items":{"type":"string"},"description":"precursor sqids"},` + position + `}}`)},
	{Name: "task_update", Description: "Change a task's title, prompt and/or type (code, bug, ux, architecture, docs, research, review, release, milestone, feature).",
		InputSchema: raw(`{"type":"object","required":["sqid"],"properties":{"sqid":{"type":"string"},"title":{"type":"string"},"prompt":{"type":"string"},` + strings.TrimSuffix(taskType, ",") + `}}`)},
	{Name: "task_move", Description: "Move a task to a new parent within your assignment and/or rerank it among its siblings.",
		InputSchema: raw(`{"type":"object","required":["sqid"],"properties":{"sqid":{"type":"string"},"parent":{"type":"string","description":"new parent sqid (omit to keep)"},` + position + `}}`)},
	{Name: "task_needs_add", Description: "Make a task depend on a precursor (any task in the project). Cycles are refused.",
		InputSchema: mcp.Schema(map[string]string{"sqid": "string", "precursor": "string"}, "sqid", "precursor")},
	{Name: "task_needs_remove", Description: "Remove a precursor dependency.",
		InputSchema: mcp.Schema(map[string]string{"sqid": "string", "precursor": "string"}, "sqid", "precursor")},
	{Name: "task_complete", Description: "Mark a leaf task you finished as complete, with a one-paragraph summary of what you did. Call this when the task you were given is done.",
		InputSchema: mcp.Schema(map[string]string{"sqid": "string", "summary": "string"}, "sqid", "summary")},
	{Name: "task_reopen", Description: "Reopen a task in your assignment.",
		InputSchema: mcp.Schema(map[string]string{"sqid": "string"}, "sqid")},
	{Name: "task_cancel", Description: "Cancel a task and its unfinished subtasks. Cancelling a task you did not create needs the owner's approval (returns pending_approval).",
		InputSchema: mcp.Schema(map[string]string{"sqid": "string", "reason": "string"}, "sqid")},
	{Name: "task_delete", Description: "Delete a task and its subtree. Deleting a task you did not create needs the owner's approval (returns pending_approval).",
		InputSchema: mcp.Schema(map[string]string{"sqid": "string"}, "sqid")},
	{Name: "ask_question", Description: "Ask the owner a question and wait for the answer. Offer options when you can; set allow_free_text for open questions.",
		InputSchema: raw(`{"type":"object","required":["prompt"],"properties":{"prompt":{"type":"string"},"detail_md":{"type":"string"},"allow_free_text":{"type":"boolean"},"options":{"type":"array","items":{"type":"object","required":["label","value"],"properties":{"label":{"type":"string"},"value":{"type":"string"},"description":{"type":"string"}}}}}}`)},
	{Name: "report_status", Description: "Report progress (shown to the owner in the TUI and web app).",
		InputSchema: mcp.Schema(map[string]string{"summary": "string", "percent": "integer"}, "summary")},
	{Name: "target_exec", Description: "Remote runs only: run a shell command on the remote target (a Mac, an Android emulator host) over SSH, or an adb shell command for Android, and return its output and exit code. The repo is checked out LOCALLY, not on the target: edit files here, copy what the target needs with target_put, and use the target to build, test and run. Fails with \"target lost\" if the machine goes away.",
		InputSchema: raw(`{"type":"object","required":["cmd"],"properties":{"cmd":{"type":"string"},"timeout_s":{"type":"integer","description":"default 600, max 3600"}}}`)},
	{Name: "target_put", Description: "Remote runs only: copy a file or directory from the local checkout to the target (scp, or adb push). path is relative to the checkout; dest is the path on the target.",
		InputSchema: mcp.Schema(map[string]string{"path": "string", "dest": "string"}, "path", "dest")},
	{Name: "submit_deliverable", Description: "Hand the owner something besides a pull request: a mockup image (kind image, image_base64 PNG/JPEG/WebP/GIF up to 5 MB), a prototype or Claude Artifact link (artifact_url), any other link (link), or a markdown report (report_md, content_md). If you cannot publish a Claude Artifact, hand back the prototype as one self-contained HTML file (prototype_html, content_html up to 1 MB; inline CSS/JS, no network) and the owner opens it in a sandboxed frame. Give each a short title. Shown on the task.",
		InputSchema: raw(`{"type":"object","required":["kind"],"properties":{"kind":{"type":"string","enum":["image","artifact_url","link","report_md","prototype_html"]},"title":{"type":"string"},"url":{"type":"string"},"content_md":{"type":"string"},"content_html":{"type":"string"},"image_base64":{"type":"string"}}}`)},
	{Name: "submit_review", Description: "Review runs only: submit your review of the pull request. risk is low, medium or high.",
		InputSchema: raw(`{"type":"object","required":["risk","summary_md"],"properties":{"risk":{"type":"string","enum":["low","medium","high"]},"summary_md":{"type":"string"},"concerns":{"type":"array","items":{"type":"string"}}}}`)},
	{Name: "submit_citation_audit", Description: "Citation audit review runs only: report, for every URL you were asked to check, whether it is verified (loads and supports the claim it is cited for), unsupported (loads but does not) or unreachable (did not load). Call once.",
		InputSchema: raw(`{"type":"object","required":["citations"],"properties":{"citations":{"type":"array","items":{"type":"object","required":["url","status"],"properties":{"url":{"type":"string"},"status":{"type":"string","enum":["verified","unsupported","unreachable"]},"note":{"type":"string","description":"what the page says instead, or why it did not load"}}}},"summary_md":{"type":"string"}}}`)},
	{Name: "propose_split", Description: "Plan runs only: propose subtasks for the task. Nested children allowed. This ends the plan run.",
		InputSchema: raw(`{"type":"object","required":["children"],"properties":{"chain":{"type":"boolean","description":"each top-level subtask needs the previous"},"rationale_md":{"type":"string"},"children":{"type":"array","items":{"type":"object","required":["title"],"properties":{"title":{"type":"string"},"prompt":{"type":"string"},"type":{"type":"string","enum":["code","bug","ux","architecture","docs","research","review","release","milestone","feature"],"description":"task type (default: the parent's, else code)"},"children":{"type":"array"}}}}}}`)},
}

// UserTools are only for the owner's own agents (the controller's /mcp endpoint).
var UserTools = []mcp.Tool{
	{Name: "project_list", Description: "List projects (slug, name, default template, auto-merge).",
		InputSchema: mcp.Schema(nil)},
	{Name: "task_queue", Description: "Queue a task (and its open subtasks) to run on a runner. Returns the run id.",
		InputSchema: mcp.Schema(map[string]string{"sqid": "string"}, "sqid")},
	{Name: "run_list", Description: "List recent runs, optionally filtered by state (queued, active, needs_input, succeeded, failed, ...) and project.",
		InputSchema: mcp.Schema(map[string]string{"state": "string", "project": "string", "limit": "integer"})},
}

// userTaskCreate replaces task_create for the owner: a top-level task needs a project.
var userTaskCreate = mcp.Tool{Name: "task_create", Description: "Create a task: top-level in a project (give project), or a subtask (give parent).",
	InputSchema: raw(`{"type":"object","required":["title"],"properties":{"project":{"type":"string","description":"project slug, for a top-level task"},"parent":{"type":"string"},"title":{"type":"string"},"prompt":{"type":"string"},"type":{"type":"string","enum":["code","bug","ux","architecture","docs","research","review","release","milestone","feature"],"description":"task type (default: the parent's, else code)"},"needs":{"type":"array","items":{"type":"string"},"description":"precursor sqids"},` + position + `}}`)}

var readTools = []string{"task_get", "task_tree", "task_list", "task_next", "ask_question", "report_status"}

// Allowed lists the tools a run of the given kind may call ("user" = the owner's own
// agents through the controller's remote MCP endpoint).
func Allowed(kind string) map[string]bool {
	m := map[string]bool{}
	for _, t := range readTools {
		m[t] = true
	}
	switch kind {
	case "work", "revise", "user":
		for _, t := range All {
			if t.Name != "submit_review" && t.Name != "propose_split" && t.Name != "submit_citation_audit" {
				m[t.Name] = true
			}
		}
		if kind == "user" {
			delete(m, "task_next")
			delete(m, "ask_question")
			delete(m, "report_status")
			for _, t := range UserTools {
				m[t.Name] = true
			}
		}
	case "review":
		m["submit_review"] = true
		m["submit_citation_audit"] = true
	case "plan":
		m["propose_split"] = true
	}
	return m
}

// For returns the tool definitions a run of this kind may call.
func For(kind string) []mcp.Tool {
	ok := Allowed(kind)
	var out []mcp.Tool
	for _, t := range All {
		if ok[t.Name] {
			if kind == "user" && t.Name == "task_create" {
				t = userTaskCreate
			}
			out = append(out, t)
		}
	}
	if kind == "user" {
		out = append(out, UserTools...)
	}
	return out
}

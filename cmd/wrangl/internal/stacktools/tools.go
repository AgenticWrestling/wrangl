// Package stacktools defines the MCP tools agents use on the work stack (spec §8). The
// runner's MCP shim and the controller's remote MCP endpoint both serve this catalogue;
// the controller enforces scope.
package stacktools

import (
	"encoding/json"

	"github.com/nreid/devhost/cmd/wrangl/internal/mcp"
)

var position = `"position": {"type": "object", "description": "where among siblings (default last)", "properties": {"first": {"type": "boolean"}, "after": {"type": "string", "description": "sibling sqid"}, "before": {"type": "string", "description": "sibling sqid"}}}`

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
		InputSchema: raw(`{"type":"object","required":["parent","title"],"properties":{"parent":{"type":"string"},"title":{"type":"string"},"prompt":{"type":"string"},"needs":{"type":"array","items":{"type":"string"},"description":"precursor sqids"},` + position + `}}`)},
	{Name: "task_update", Description: "Change a task's title and/or prompt.",
		InputSchema: mcp.Schema(map[string]string{"sqid": "string", "title": "string", "prompt": "string"}, "sqid")},
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
	{Name: "submit_review", Description: "Review runs only: submit your review of the pull request. risk is low, medium or high.",
		InputSchema: raw(`{"type":"object","required":["risk","summary_md"],"properties":{"risk":{"type":"string","enum":["low","medium","high"]},"summary_md":{"type":"string"},"concerns":{"type":"array","items":{"type":"string"}}}}`)},
	{Name: "propose_split", Description: "Plan runs only: propose subtasks for the task. Nested children allowed. This ends the plan run.",
		InputSchema: raw(`{"type":"object","required":["children"],"properties":{"chain":{"type":"boolean","description":"each top-level subtask needs the previous"},"rationale_md":{"type":"string"},"children":{"type":"array","items":{"type":"object","required":["title"],"properties":{"title":{"type":"string"},"prompt":{"type":"string"},"children":{"type":"array"}}}}}}`)},
}

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
			if t.Name != "submit_review" && t.Name != "propose_split" {
				m[t.Name] = true
			}
		}
		if kind == "user" {
			delete(m, "task_next")
			delete(m, "ask_question")
			delete(m, "report_status")
		}
	case "review":
		m["submit_review"] = true
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
			out = append(out, t)
		}
	}
	return out
}

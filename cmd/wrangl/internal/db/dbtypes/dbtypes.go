// Package dbtypes holds Go types that sqlc maps database columns onto (sqlc.yaml
// overrides). SQLite has no enum type: each enum is a TEXT column with a CHECK constraint
// in the migration and a string type here; keep the two lists in step.
package dbtypes

import "fmt"

// TaskType says what kind of work a task is. It picks the icon in the UIs and adds a
// line of guidance to the agent's prompt.
type TaskType string

const (
	TaskCode         TaskType = "code" // the default: change the code
	TaskUX           TaskType = "ux"
	TaskArchitecture TaskType = "architecture"
	TaskSpec         TaskType = "spec" // spec only: no code changes
	TaskDocs         TaskType = "docs"
	TaskReview       TaskType = "review"
)

// TaskTypes is every type, in display order.
var TaskTypes = []TaskType{TaskCode, TaskUX, TaskArchitecture, TaskSpec, TaskDocs, TaskReview}

var taskTypeLabels = map[TaskType]string{
	TaskCode: "Code", TaskUX: "UX design", TaskArchitecture: "Architecture",
	TaskSpec: "Spec only", TaskDocs: "Docs update", TaskReview: "Code review",
}

// Label is the human name.
func (t TaskType) Label() string { return taskTypeLabels[t] }

// Valid reports whether t is a known type.
func (t TaskType) Valid() bool { _, ok := taskTypeLabels[t]; return ok }

// ParseTaskType accepts a type name; "" means the default (code).
func ParseTaskType(s string) (TaskType, error) {
	if s == "" {
		return TaskCode, nil
	}
	t := TaskType(s)
	if !t.Valid() {
		return "", fmt.Errorf("task type %q: want one of code, ux, architecture, spec, docs, review", s)
	}
	return t, nil
}

// Guidance is the line added to an agent's prompt for this type ("" for code).
func (t TaskType) Guidance() string {
	switch t {
	case TaskUX:
		return "This is a UX design task: focus on user flows, layout and interaction; produce mockups, component structure or UI changes as the task asks."
	case TaskArchitecture:
		return "This is an architecture task: decide structure, boundaries and trade-offs; record the decision (for example as an ADR or design doc) before or instead of large code changes."
	case TaskSpec:
		return "This is a spec-only task: write or update the specification document. Do not change application code."
	case TaskDocs:
		return "This is a docs update: change documentation only (README, guides, comments) to match the current behaviour."
	case TaskReview:
		return "This is a code review task: read the code in question and report findings with file:line references; do not rewrite it unless the task asks you to."
	}
	return ""
}

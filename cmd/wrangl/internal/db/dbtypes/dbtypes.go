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
	TaskSpec         TaskType = "spec" // deprecated: parses to docs; kept so old rows stay valid
	TaskBug          TaskType = "bug"
	TaskResearch     TaskType = "research" // non-code knowledge work: a report with sources
	TaskDocs         TaskType = "docs"
	TaskReview       TaskType = "review"
	// Planning types: they group work rather than being done themselves, and are drawn
	// bold. A release holds milestones; a milestone holds features and loose tasks; a
	// feature holds tasks. Nothing enforces that nesting.
	TaskRelease   TaskType = "release"
	TaskMilestone TaskType = "milestone"
	TaskFeature   TaskType = "feature"
)

// TaskTypes is every type, in display order.
var TaskTypes = []TaskType{TaskCode, TaskBug, TaskUX, TaskArchitecture, TaskDocs, TaskResearch, TaskReview, TaskRelease, TaskMilestone, TaskFeature}

var taskTypeLabels = map[TaskType]string{
	TaskCode: "Code", TaskUX: "UX design", TaskArchitecture: "Architecture",
	TaskSpec: "Docs", TaskDocs: "Docs", TaskBug: "Bug fix", TaskResearch: "Research", TaskReview: "Code review",
	TaskRelease: "Release", TaskMilestone: "Milestone", TaskFeature: "Feature",
}

// OwnerGate reports whether a pull request for a task of this type always waits for the
// owner, however the project's auto-merge is set: designs and decisions are judgment calls.
func (t TaskType) OwnerGate() bool { return t == TaskUX || t == TaskArchitecture }

// Planning reports whether t is a release, milestone or feature.
func (t TaskType) Planning() bool { return t == TaskRelease || t == TaskMilestone || t == TaskFeature }

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
	if t == TaskSpec {
		return TaskDocs, nil
	}
	if !t.Valid() {
		return "", fmt.Errorf("task type %q: want one of code, bug, ux, architecture, docs, research, review, release, milestone, feature", s)
	}
	return t, nil
}

// researchContract is the prompt contract for research tasks (and what the report page parses:
// the "Sources" heading becomes the Sources panel).
const researchContract = `This is a research task: non-code knowledge work. Do not change application code.
Write the report as ONE markdown file at research/<task sqid>-<short-slug>.md in the repo you were given (the project's notes repo; commit only under research/ so the PR can merge itself), AND submit the same markdown with submit_deliverable (kind report_md, a short title) so the owner reads it on the task page.
Use exactly these level-2 headings, in this order:
## Question - the question as you understood it, in one or two sentences.
## Recommendation - first, at most ten lines: what to do and why. Lead with the answer, not the method.
## Analysis - the reasoning; options compared; numbers with units.
## Sources - a numbered list. Each entry: title, URL or document, the date you accessed it and the date it was published or last updated, and the passage you relied on. Cite them in the text as [1], [2]. Every factual claim needs a source; never invent one. Say plainly what you could not find or verify.
## Assumptions - what you took as given.
## Open questions - what would change the answer.
## Confidence - high, medium or low, with one sentence of why.
For legal topics, open the Recommendation with the jurisdiction (country, state/province) the answer applies to, and state that this is research for counsel to review and not legal advice.`

// Guidance is the block added to an agent's prompt for this type ("" for code).
func (t TaskType) Guidance() string {
	switch t {
	case TaskBug:
		return "This is a bug fix. First reproduce the defect and write a failing test that captures it; then fix the cause (not the symptom) and show the test passes. State the root cause in your completion summary."
	case TaskUX:
		return "This is a UX design task: focus on user flows, layout and interaction. Produce two or three variants, and for each save a mockup image (PNG at desktop and phone widths) under docs/design/<task sqid>/. If you can publish an interactive prototype (a Claude Artifact), include its link in your completion summary; otherwise commit a self-contained HTML prototype beside the images. Do not change application code unless the task asks. The owner reviews the visuals, not a code diff."
	case TaskArchitecture:
		return "This is an architecture task: decide structure, boundaries and trade-offs. Write an ADR (context, options with at least two real alternatives, decision, consequences) as a markdown document in the repo's docs. Do not change application code. The owner will read and decide before it merges."
	case TaskDocs:
		return "This is a documentation task (README, guides, specifications, comments). Change documentation only, and verify every command, path and behaviour you describe against the code."
	case TaskResearch:
		return researchContract
	case TaskReview:
		return "This is a code review task: read the code in question and report severity-ranked findings with file:line references; do not rewrite it unless the task asks you to."
	case TaskRelease:
		return "This task is a release: a container for milestones. Its prompt states what shipping it means; work happens in its subtasks, not here."
	case TaskMilestone:
		return "This task is a milestone: a container for features and loose tasks. Its prompt states the definition of done; work happens in its subtasks, not here."
	case TaskFeature:
		return "This task is a feature: a container for the tasks that deliver it. Its prompt states the goal and acceptance criteria; work happens in its subtasks, not here."
	}
	return ""
}

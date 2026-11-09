package proto

import (
	"encoding/json"
	"time"
)

// RunnerView is a runner as the API returns it.
type RunnerView struct {
	ID              int64           `json:"id"`
	Name            string          `json:"name"`
	State           string          `json:"state"`
	Template        string          `json:"template"`
	Launched        bool            `json:"launched"` // false = adopted via enroll-token
	InstanceID      string          `json:"instance_id,omitempty"`
	InstanceType    string          `json:"instance_type,omitempty"`
	AZ              string          `json:"az,omitempty"`
	PrivateIP       string          `json:"private_ip,omitempty"`
	Arch            string          `json:"arch,omitempty"`
	VCPU            int64           `json:"vcpu,omitempty"`
	RAMMiB          int64           `json:"ram_mib,omitempty"`
	DiskGiB         int64           `json:"disk_gib,omitempty"`
	LastHeartbeatAt string          `json:"last_heartbeat_at,omitempty"`
	IdleSince       string          `json:"idle_since,omitempty"`
	IdleTimeoutS    int64           `json:"idle_timeout_s,omitempty"`
	LaunchedAt      string          `json:"launched_at,omitempty"`
	EnrolledAt      string          `json:"enrolled_at,omitempty"`
	TerminatedAt    string          `json:"terminated_at,omitempty"`
	Metrics         json.RawMessage `json:"metrics,omitempty"`
	Cordoned        bool            `json:"cordoned"`
	Draining        bool            `json:"draining"`
	TemplateVersion int64           `json:"template_version"` // applied on the runner
	WantVersion     int64           `json:"want_version,omitempty"`
	LastError       string          `json:"last_error,omitempty"`
	TemplateLog     string          `json:"template_log,omitempty"`
	BinarySHA       string          `json:"binary_sha,omitempty"`  // running wrangl binary
	UpgradeSHA      string          `json:"upgrade_sha,omitempty"` // pending self-upgrade target
	Capacity        *CapacityView   `json:"capacity,omitempty"`
}

// CapacityView is what a runner can hold and what its active runs reserved (ADR 0009 section 6).
type CapacityView struct {
	VCPU        int64  `json:"vcpu"`
	RAMMiB      int64  `json:"ram_mib"`
	DiskGiB     int64  `json:"disk_gib"`
	UsedVCPU    int64  `json:"used_vcpu"`
	UsedRAMMiB  int64  `json:"used_ram_mib"`
	UsedDiskGiB int64  `json:"used_disk_gib"`
	MaxSessions *int64 `json:"max_sessions,omitempty"`
	Explicit    bool   `json:"explicit,omitempty"` // set by the owner rather than derived from the machine
	// GuardClosed: the measured guard has closed the runner to new work; GuardReason says why.
	GuardClosed bool   `json:"guard_closed,omitempty"`
	GuardReason string `json:"guard_reason,omitempty"`
}

// RunnerPatch is PATCH /api/v1/runners/{name}: Capacity sets fields (vcpu, ram, disk); an empty value
// goes back to deriving that field from the machine.
type RunnerPatch struct {
	Capacity map[string]string `json:"capacity,omitempty"`
	// Guard sets the measured-guard thresholds: load1, mem (percent), disk (percent); an empty value stops
	// guarding that measure.
	Guard map[string]string `json:"guard,omitempty"`
}

// TemplateView is a runner template as the API returns it.
type TemplateView struct {
	Name          string          `json:"name"`
	Description   string          `json:"description,omitempty"`
	Arch          string          `json:"arch"`
	MinVCPU       int64           `json:"min_vcpu"`
	MinRAMMiB     int64           `json:"min_ram_mib"`
	MinDiskGiB    int64           `json:"min_disk_gib"`
	InstanceTypes []string        `json:"instance_types"`
	AMI           string          `json:"ami"`
	MaxRunners    int64           `json:"max_runners"`
	MaxSessions   int64           `json:"max_sessions"`
	IdleTimeoutS  int64           `json:"idle_timeout_s"`
	Version       int64           `json:"version"`
	FailedVersion int64           `json:"failed_version,omitempty"`
	MiseTOML      string          `json:"mise_toml"`
	SetupScript   string          `json:"setup_script,omitempty"`
	Agent         json.RawMessage `json:"agent"`
}

// LaunchRequest is POST /api/v1/runners.
type LaunchRequest struct {
	Template string `json:"template"`
	Count    int    `json:"count"`
}

// EnrollTokenRequest is POST /api/v1/enroll-tokens.
type EnrollTokenRequest struct {
	Template string `json:"template"`
	TTLS     int64  `json:"ttl_s"`
}

// EnrollTokenResponse carries the token and the command to run on the box.
type EnrollTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	EnrollURL string    `json:"enroll_url"`
	Command   string    `json:"command"`
}

// APIError is the body of every non-2xx response.
type APIError struct {
	Error string `json:"error"`
}

// ProjectView is a project as the API returns it.
type ProjectView struct {
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Template     string `json:"template,omitempty"`
	PlanTemplate string `json:"plan_template,omitempty"`
	AutoMerge    bool   `json:"auto_merge"`
	AutoQueue    bool   `json:"auto_queue"`
	// AutoResolve: auto-merge queues Smart Merge for a conflicted PR (otherwise it asks the owner).
	AutoResolve bool   `json:"auto_resolve"`
	Priority    int64  `json:"priority"`
	NotesRepo   string `json:"notes_repo,omitempty"`
	Agent       string `json:"agent,omitempty"` // default agent definition
	// AllowTemplates: other templates (usually host) this project's runs may also use.
	AllowTemplates []string `json:"allow_templates,omitempty"`
	// Need is the effective resource need set for the project's tasks (default, or the project's own).
	Need *NeedView `json:"need,omitempty"`
	// CitationAudit: research PRs get an audit run that re-fetches every cited URL before merge.
	CitationAudit bool `json:"citation_audit"`
	// Archived is set when the project is archived (hidden and inert); ArchivedAt is when.
	Archived   bool   `json:"archived,omitempty"`
	ArchivedAt string `json:"archived_at,omitempty"`
}

// ProjectArchiveRefusal is the 409 body when a project cannot be archived: runs are active.
type ProjectArchiveRefusal struct {
	Error string                  `json:"error"`
	Runs  []ProjectArchiveRunView `json:"runs"`
}

// ProjectArchiveRunView is an active run that blocks archiving.
type ProjectArchiveRunView struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
	Task  string `json:"task"`
	Title string `json:"title"`
}

// ProjectArchiveResult is the answer to archiving a project.
type ProjectArchiveResult struct {
	Project       ProjectView `json:"project"`
	CancelledRuns []int64     `json:"cancelled_runs"` // queued runs cancelled with 'project archived'
}

// ProjectRequest creates or patches a project (nil = unchanged).
type ProjectRequest struct {
	Slug           string            `json:"slug,omitempty"`
	Name           *string           `json:"name,omitempty"`
	Description    *string           `json:"description,omitempty"`
	Template       *string           `json:"template,omitempty"`
	PlanTemplate   *string           `json:"plan_template,omitempty"`
	AllowTemplates *[]string         `json:"allow_templates,omitempty"` // replaces the list; empty clears
	Need           map[string]string `json:"need,omitempty"`            // resource needs by field; empty value clears
	AutoMerge      *bool             `json:"auto_merge,omitempty"`
	AutoQueue      *bool             `json:"auto_queue,omitempty"`
	AutoResolve    *bool             `json:"auto_resolve,omitempty"`
	Priority       *int64            `json:"priority,omitempty"`
	NotesRepo      *string           `json:"notes_repo,omitempty"` // owner/name of a linked repo; "" clears
	Agent          *string           `json:"agent,omitempty"`      // default agent name; "" clears
	CitationAudit  *bool             `json:"citation_audit,omitempty"`
}

// AgentView is a named agent definition (docs/wrangl/adr/0001).
type AgentView struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Version     int64           `json:"version"`
	Spec        json.RawMessage `json:"spec"`
	Templates   []string        `json:"templates"` // templates that allow it
}

// AgentRequest creates or replaces an agent definition.
type AgentRequest struct {
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Spec        json.RawMessage `json:"spec"`
}

// QueueRequest is the optional body of POST /tasks/{sqid}/queue.
type QueueRequest struct {
	Agent string `json:"agent,omitempty"` // override the project's default agent for this run
}

// RepoView is a linked repo.
type RepoView struct {
	Project       string `json:"project"`
	Repo          string `json:"repo"`
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
}

// LinkRepoRequest links owner/name to a project.
type LinkRepoRequest struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
}

// IssueView is a synced issue.
type IssueView struct {
	Ref   string `json:"ref"` // owner/name#n
	Title string `json:"title"`
	State string `json:"state"`
	URL   string `json:"url"`
	Body  string `json:"body,omitempty"`
	Task  string `json:"task,omitempty"` // sqid of a task made from it
}

// TaskNode is one row of a ranked tree.
type TaskNode struct {
	Sqid   string `json:"sqid"`
	Parent string `json:"parent,omitempty"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Type   string `json:"type"`
	Target string `json:"target_date,omitempty"` // YYYY-MM-DD
	// Planning types only: leaves done / total beneath, and "at_risk" or "late" against the target date.
	Done     int64  `json:"leaves_done,omitempty"`
	Total    int64  `json:"leaves_total,omitempty"`
	Slip     string `json:"slip,omitempty"`
	Depth    int64  `json:"depth"`
	Unmet    int64  `json:"unmet_precursors,omitempty"`
	Children int64  `json:"children,omitempty"`
	LockedBy string `json:"locked_by,omitempty"` // runner working on its subtree
}

// TaskRef is a task in a list (precursor, dependent, child).
type TaskRef struct {
	Sqid  string `json:"sqid"`
	Title string `json:"title"`
	State string `json:"state"`
}

// TaskEventView is one audit entry.
type TaskEventView struct {
	TS    string          `json:"ts"`
	Actor string          `json:"actor"`
	Op    string          `json:"op"`
	Diff  json.RawMessage `json:"diff,omitempty"`
}

// TaskRunView is a run of a task.
type TaskRunView struct {
	ID       int64  `json:"id"`
	State    string `json:"state"`
	Runner   string `json:"runner,omitempty"`
	Branch   string `json:"branch"`
	Pushed   bool   `json:"pushed"`
	HeadSHA  string `json:"head_sha,omitempty"`
	QueuedAt string `json:"queued_at"`
}

// TaskDetail is GET /api/v1/tasks/{sqid}.
type TaskDetail struct {
	Sqid       string          `json:"sqid"`
	Project    string          `json:"project"`
	Parent     string          `json:"parent,omitempty"`
	Title      string          `json:"title"`
	Prompt     string          `json:"prompt"`
	State      string          `json:"state"`
	Type       string          `json:"type"`
	TargetDate string          `json:"target_date,omitempty"` // YYYY-MM-DD
	CreatedBy  string          `json:"created_by"`
	Summary    string          `json:"summary,omitempty"`
	Need       *NeedView       `json:"need,omitempty"`
	Children   []TaskRef       `json:"children"`
	Precursors []TaskRef       `json:"precursors"`
	Dependents []TaskRef       `json:"dependents"`
	Runs       []TaskRunView   `json:"runs"`
	Events     []TaskEventView `json:"events"`
}

// NeedView is an effective resource need set: formatted values and which layer each came from
// (default, project or task).
type NeedView struct {
	VCPU   string            `json:"vcpu"`
	RAM    string            `json:"ram"`
	Disk   string            `json:"disk"`
	Source map[string]string `json:"source,omitempty"`
}

// PlacementView is GET /api/v1/placement: the global default need set.
type PlacementView struct {
	Need NeedView `json:"need"`
	// Budget is the hourly cost ceiling in USD (nil = none); Committed is what runners that still bill
	// cost per hour; Approaching is set at or above the warning share of the budget.
	Budget      *float64 `json:"budget_hourly_usd,omitempty"`
	WarnPct     int64    `json:"budget_warn_pct,omitempty"`
	Committed   float64  `json:"committed_hourly_usd"`
	Approaching bool     `json:"approaching,omitempty"`
	// NeedsInputGraceS: a run waiting on the owner longer than this stops reserving vCPU (0 = off).
	NeedsInputGraceS int64 `json:"needs_input_grace_s,omitempty"`
}

// PlacementPatch is PATCH /api/v1/placement: set default needs, e.g. {"ram": "2GiB"}.
type PlacementPatch struct {
	Need map[string]string `json:"need,omitempty"`
	// Budget sets the hourly budget in USD ("0.50"); "none" removes it; nil leaves it.
	Budget *string `json:"budget,omitempty"`
	// NeedsInputGrace ("10m", or "off") sets how long a needs_input run keeps its vCPU reserved.
	NeedsInputGrace *string `json:"needs_input_grace,omitempty"`
}

// TaskCreateRequest is POST /api/v1/tasks.
type TaskCreateRequest struct {
	Project  string   `json:"project"`
	Parent   string   `json:"parent,omitempty"`
	Title    string   `json:"title"`
	Prompt   string   `json:"prompt,omitempty"`
	First    bool     `json:"first,omitempty"`
	After    string   `json:"after,omitempty"`
	Before   string   `json:"before,omitempty"`
	Repo     string   `json:"repo,omitempty"`
	Template string   `json:"template,omitempty"`
	Issue    string   `json:"issue,omitempty"`
	Needs    []string `json:"needs,omitempty"`
	// Need sets resource needs, e.g. {"ram": "8GiB"} (see ADR 0009).
	Need map[string]string `json:"need,omitempty"`
	Type string            `json:"type,omitempty"` // code | ux | architecture | spec | docs | review | release | milestone | feature
	// TargetDate is YYYY-MM-DD.
	TargetDate string `json:"target_date,omitempty"`
}

// TaskPatchRequest edits title/prompt/type.
type TaskPatchRequest struct {
	Title  *string `json:"title,omitempty"`
	Prompt *string `json:"prompt,omitempty"`
	Type   *string `json:"type,omitempty"`
	// Need changes resource needs; an empty value clears the field back to inherit.
	Need map[string]string `json:"need,omitempty"`
	// TargetDate: YYYY-MM-DD; "" clears it; nil leaves it.
	TargetDate *string `json:"target_date,omitempty"`
}

// TaskMoveRequest moves/reranks a task. Parent nil + Root false = keep parent.
type TaskMoveRequest struct {
	Parent *string `json:"parent,omitempty"`
	Root   bool    `json:"root,omitempty"`
	First  bool    `json:"first,omitempty"`
	After  string  `json:"after,omitempty"`
	Before string  `json:"before,omitempty"`
}

// SplitNode is a subtask in a split request.
type SplitNode struct {
	Title    string      `json:"title"`
	Prompt   string      `json:"prompt,omitempty"`
	Type     string      `json:"type,omitempty"`
	Children []SplitNode `json:"children,omitempty"`
}

// TaskSplitRequest splits a task: children, or an outline to parse.
type TaskSplitRequest struct {
	Children   []SplitNode `json:"children,omitempty"`
	Outline    string      `json:"outline,omitempty"`
	Chain      bool        `json:"chain,omitempty"`
	CopyPrompt bool        `json:"copy_prompt,omitempty"`
}

// MessageRequest carries a reason/summary.
type MessageRequest struct {
	Message string `json:"message,omitempty"`
}

// RunView is a run as the API returns it.
type RunView struct {
	ID           int64  `json:"id"`
	Task         string `json:"task"`
	TaskTitle    string `json:"task_title"`
	Project      string `json:"project"`
	Kind         string `json:"kind"`
	State        string `json:"state"`
	Runner       string `json:"runner,omitempty"`
	BlockedSince string `json:"blocked_since,omitempty"` // queued runs: since when the status says why it waits
	Current      string `json:"current,omitempty"`       // sqid of the leaf in progress
	Branch       string `json:"branch"`
	Session      string `json:"session,omitempty"`
	Status       string `json:"status,omitempty"`
	Pushed       bool   `json:"pushed"`
	BaseSHA      string `json:"base_sha,omitempty"`
	HeadSHA      string `json:"head_sha,omitempty"`
	Error        string `json:"error,omitempty"`
	QueuedAt     string `json:"queued_at"`
	StartedAt    string `json:"started_at,omitempty"`
	FinishedAt   string `json:"finished_at,omitempty"`
}

// LogView is a stored log line.
type LogView struct {
	ID      int64  `json:"id"`
	RunID   int64  `json:"run_id"`
	TS      string `json:"ts"`
	Stream  string `json:"stream"`
	Message string `json:"message"`
	Project string `json:"project,omitempty"`
	Runner  string `json:"runner,omitempty"`
	Task    string `json:"task,omitempty"`
}

// PRView is a pull request as the API returns it.
type PRView struct {
	ID       int64           `json:"id"`
	Number   int64           `json:"number"`
	URL      string          `json:"url"`
	Repo     string          `json:"repo"`
	Task     string          `json:"task"`
	Title    string          `json:"title"`
	Branch   string          `json:"branch"`
	State    string          `json:"state"`
	Risk     string          `json:"risk,omitempty"`
	Decision string          `json:"decision,omitempty"`
	Checks   string          `json:"checks,omitempty"`
	Gates    json.RawMessage `json:"gates,omitempty"`
	Review   string          `json:"review,omitempty"`
}

// TargetView is a remote target (a machine without a wrangl binary, owned by the controller).
type TargetView struct {
	Name        string `json:"name"`
	Template    string `json:"template"`
	State       string `json:"state"` // launching | ready | offline | draining | terminated
	InstanceID  string `json:"instance_id,omitempty"`
	Sessions    int64  `json:"sessions"` // runs using it now
	MaxSessions int64  `json:"max_sessions"`
	IdleSince   string `json:"idle_since,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	Project     string `json:"project,omitempty"` // the project whose demand launched it
}

// ProjectRunner is a runner with at least one active session for a project (GET /api/v1/projects/{slug}/runners).
// A runner may serve several projects: AlsoServing names the others, so the UI can tag it as shared.
type ProjectRunner struct {
	Name        string           `json:"name"`
	State       string           `json:"state"`
	Template    string           `json:"template"`
	Sessions    []ProjectSession `json:"sessions"`
	AlsoServing []string         `json:"also_serving,omitempty"`
}

// ProjectSession is one active run of the project on a runner.
type ProjectSession struct {
	Run   int64  `json:"run"`
	Task  string `json:"task"`
	Title string `json:"title"`
	State string `json:"state"`
}

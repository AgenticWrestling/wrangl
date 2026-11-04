package proto

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// AgentSpec is a template's agent block (spec §8).
type AgentSpec struct {
	Drive       string            `json:"drive,omitempty"` // headless (M4) | interactive (M9)
	Start       []string          `json:"start"`
	Resume      []string          `json:"resume,omitempty"` // default: Start
	Env         map[string]string `json:"env,omitempty"`
	Secrets     map[string]string `json:"secrets,omitempty"` // ENV -> controller secret name; resolved by the controller
	TurnTimeout string            `json:"turn_timeout,omitempty"`
	// Completion: "tool" (default) = the agent calls task_complete; "exit" = a turn that
	// exits 0 completes its task (agents without MCP).
	Completion string      `json:"completion,omitempty"`
	Files      []AgentFile `json:"files,omitempty"`
	// SecretFiles are written (0600, as the agent) before the first turn and removed when
	// the run ends: login files an agent can only read from disk, such as a full Claude
	// Code login for Remote Control, or Codex's auth.json. Resolved by the controller.
	SecretFiles []SecretFileSpec `json:"secret_files,omitempty"`
	// Expect answers prompts in the PTY (interactive drive): when the ANSI-stripped output
	// matches, Send is typed. Each rule fires once unless Repeat is set.
	Expect []ExpectRule `json:"expect,omitempty"`
}

// Drive modes.
const (
	DriveHeadless    = "headless"    // one agent process per turn (M4)
	DriveInteractive = "interactive" // one session; the Stop hook chains the next task (M9)
)

// SecretFileSpec names a controller secret to materialise as a file on the runner.
type SecretFileSpec struct {
	Path   string `json:"path"`   // templated; relative paths are inside the workspace
	Secret string `json:"secret"` // controller secret name
	Mode   string `json:"mode,omitempty"`
}

// SecretFile is a resolved SecretFileSpec in an assignment.
type SecretFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    string `json:"mode,omitempty"`
}

// ExpectRule is one PTY prompt and its answer.
type ExpectRule struct {
	Match  string `json:"match"` // Go regexp
	Send   string `json:"send"`
	Repeat bool   `json:"repeat,omitempty"`
}

// Validate checks the parts of an agent block the runner would otherwise fail on late.
func (a AgentSpec) Validate() error {
	switch a.Drive {
	case "", DriveHeadless, DriveInteractive:
	default:
		return fmt.Errorf("agent.drive %q: want headless or interactive", a.Drive)
	}
	for _, f := range a.Files {
		if f.Merge != "" && f.Merge != "json" {
			return fmt.Errorf("agent.files %s: merge %q: want json", f.Path, f.Merge)
		}
	}
	for _, e := range a.Expect {
		if _, err := regexp.Compile(e.Match); err != nil {
			return fmt.Errorf("agent.expect %q: %w", e.Match, err)
		}
	}
	for _, f := range a.SecretFiles {
		if f.Path == "" || f.Secret == "" {
			return fmt.Errorf("agent.secret_files: path and secret are required")
		}
		if f.Mode != "" {
			if _, err := strconv.ParseUint(f.Mode, 8, 32); err != nil {
				return fmt.Errorf("agent.secret_files mode %q: want octal like 0600", f.Mode)
			}
		}
	}
	return nil
}

// AgentFile is written (templated) into the workspace before the first turn.
type AgentFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// Merge "json" deep-merges Content (a JSON object) into the existing file instead of
	// replacing it: for an agent's own state files such as ~/.claude.json.
	Merge string `json:"merge,omitempty"`
}

// ParseAgentSpec decodes a template's agent_json.
func ParseAgentSpec(raw string) (AgentSpec, error) {
	var a AgentSpec
	if raw == "" || raw == "{}" {
		return a, nil
	}
	err := json.Unmarshal([]byte(raw), &a)
	return a, err
}

// Assignment is the cmd.assign payload.
type Assignment struct {
	RunID       int64             `json:"run_id"`
	Kind        string            `json:"kind"`
	TaskSqid    string            `json:"task_sqid"`
	Repo        string            `json:"repo"` // owner/name
	CloneURL    string            `json:"clone_url"`
	BaseBranch  string            `json:"base_branch"`
	Branch      string            `json:"branch"`
	Continue    bool              `json:"continue,omitempty"`  // Branch already exists (revise / continuation): clone it
	GitToken    string            `json:"git_token,omitempty"` // installation token; daemon-only, never given to the agent
	SessionName string            `json:"session_name"`
	SessionUUID string            `json:"session_uuid"`
	Agent       AgentSpec         `json:"agent"`
	Env         map[string]string `json:"env,omitempty"` // resolved secrets for the agent process
	SecretFiles []SecretFile      `json:"secret_files,omitempty"`
}

// Ack is a generic accepted/declined reply.
type Ack struct {
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
}

// RunState is published (JetStream) on wr.evt.<r>.<run>.state.
type RunState struct {
	State    string `json:"state"` // preparing|running|pushing|succeeded|failed|blocked|cancelled
	Status   string `json:"status,omitempty"`
	BaseSHA  string `json:"base_sha,omitempty"`
	HeadSHA  string `json:"head_sha,omitempty"`
	Pushed   bool   `json:"pushed,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Terminal reports whether a run state is final.
func Terminal(state string) bool {
	return state == "succeeded" || state == "failed" || state == "blocked" || state == "cancelled"
}

// LogLine is published (JetStream) on wr.evt.<r>.<run>.log.
type LogLine struct {
	Seq     int64     `json:"seq"`
	TS      time.Time `json:"ts"`
	Stream  string    `json:"stream"` // agent | runner
	Message string    `json:"message"`
}

// StackRequest is sent on wr.stack.<r>.<op> (op: next | complete).
type StackRequest struct {
	RunID   int64  `json:"run_id"`
	Sqid    string `json:"sqid,omitempty"`
	Summary string `json:"summary,omitempty"`
	Commit  string `json:"commit,omitempty"` // checkpoint: the commit checked out after the task's checkpoint
}

// StackNext describes the next leaf (or that there is none).
type StackNext struct {
	Done    bool   `json:"done"`
	Blocked bool   `json:"blocked,omitempty"`
	Sqid    string `json:"sqid,omitempty"`
	Title   string `json:"title,omitempty"`
	Prompt  string `json:"prompt,omitempty"`
}

// StackReply answers a StackRequest.
type StackReply struct {
	OK    bool       `json:"ok"`
	Error string     `json:"error,omitempty"`
	Next  *StackNext `json:"next,omitempty"`
}

// RunRef addresses a run on a runner (cmd.cancel, cmd.diff, cmd.tree, cmd.file).
type RunRef struct {
	RunID int64  `json:"run_id"`
	Path  string `json:"path,omitempty"`
	Stat  bool   `json:"stat,omitempty"`
}

// InspectReply carries diff/tree/file output.
type InspectReply struct {
	Data      string `json:"data"`
	Truncated bool   `json:"truncated,omitempty"`
	Error     string `json:"error,omitempty"`
}

// InspectLimit caps diff/tree/file replies.
const InspectLimit = 4 << 20

// ToolRequest is a work-stack tool call relayed by a runner daemon (wr.stack.<r>.tool).
type ToolRequest struct {
	RunID int64           `json:"run_id"`
	Tool  string          `json:"tool"`
	Args  json.RawMessage `json:"args"`
}

// ToolReply answers a ToolRequest.
type ToolReply struct {
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// Answer is sent to a runner (cmd.answer) when a question it asked is answered.
type Answer struct {
	QuestionID int64  `json:"question_id"`
	Value      string `json:"value,omitempty"`
	Text       string `json:"text,omitempty"`
	Cancelled  bool   `json:"cancelled,omitempty"`
}

// QuestionOption is one choice offered with a question.
type QuestionOption struct {
	Label       string `json:"label"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

// QuestionView is a question as the API returns it.
type QuestionView struct {
	ID            int64            `json:"id"`
	Kind          string           `json:"kind"`
	State         string           `json:"state"`
	Task          string           `json:"task"`
	TaskTitle     string           `json:"task_title"`
	Project       string           `json:"project"`
	RunID         int64            `json:"run_id,omitempty"`
	Prompt        string           `json:"prompt"`
	DetailMD      string           `json:"detail_md,omitempty"`
	AllowFreeText bool             `json:"allow_free_text"`
	Options       []QuestionOption `json:"options"`
	Payload       json.RawMessage  `json:"payload,omitempty"`
	Answer        string           `json:"answer,omitempty"`
	CreatedAt     string           `json:"created_at"`
}

// AnswerRequest is POST /api/v1/questions/{id}/answer.
type AnswerRequest struct {
	Value string `json:"value,omitempty"`
	Text  string `json:"text,omitempty"`
}

// TermInfo describes one terminal session on a runner: the PTY of the run in progress
// ("run-<id>") or a login shell the owner opened ("sh-<n>").
type TermInfo struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"` // run | shell
	Title string `json:"title"`
	Cols  uint16 `json:"cols"`
	Rows  uint16 `json:"rows"`
}

// TermSize is the cmd.term.size payload.
type TermSize struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// TermOpenReply answers cmd.shell.open.
type TermOpenReply struct {
	ID    string `json:"id,omitempty"`
	Error string `json:"error,omitempty"`
}

// TermEndHeader marks the final (empty) message of a terminal's output on the PTY stream.
const TermEndHeader = "Wrangl-Term-End"

package proto

import (
	"encoding/json"
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
}

// AgentFile is written (templated) into the workspace before the first turn.
type AgentFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
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
	GitToken    string            `json:"git_token,omitempty"` // installation token; daemon-only, never given to the agent
	SessionName string            `json:"session_name"`
	SessionUUID string            `json:"session_uuid"`
	Agent       AgentSpec         `json:"agent"`
	Env         map[string]string `json:"env,omitempty"` // resolved secrets for the agent process
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

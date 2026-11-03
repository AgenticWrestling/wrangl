// Package agents runs controller agents: lightweight agent CLIs on the controller host that
// answer questions about a task and its subtree ("Ask?") and may make small edits through the
// wrangl CLI. They are deliberately boxed in: a few read tools and the wrangl command, a turn
// and time limit, low scheduling priority and a small concurrency cap, so the controller stays
// responsive. Builds, tests and anything heavy belong on runners.
package agents

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nreid/devhost/cmd/wrangl/internal/db"
	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbgen"
	"github.com/nreid/devhost/cmd/wrangl/internal/errs"
	"github.com/nreid/devhost/cmd/wrangl/internal/events"
	"github.com/nreid/devhost/cmd/wrangl/internal/stack"
)

// Spec is one invocation of an agent CLI.
type Spec struct {
	Agent     dbgen.ControllerAgent
	Prompt    string
	System    string
	SessionID string // resume this session when set
	Env       []string
	Dir       string
}

// Result is what the CLI reported.
type Result struct {
	Text      string
	SessionID string
}

// Service owns the registry, the threads and the execution of asks.
type Service struct {
	DB    *db.DB
	Stack *stack.Service
	Bus   *events.Bus
	Log   *slog.Logger
	// Secret reads a named secret (for agents with auth "secret").
	Secret func(name string) ([]byte, error)
	// Exec runs a spec; nil uses the real CLI. Tests replace it.
	Exec func(ctx context.Context, sp Spec) (Result, error)
	// Dir is the working directory agents start in (default: the user's home).
	Dir string

	sem chan struct{}
}

// MaxConcurrent caps simultaneous agent processes on the controller host.
const MaxConcurrent = 2

// maxContext bounds the task-tree brief sent with each question.
const maxContext = 24 << 10

func (s *Service) slots() chan struct{} {
	if s.sem == nil {
		s.sem = make(chan struct{}, MaxConcurrent)
	}
	return s.sem
}

// Recover marks asks interrupted by a restart as failed. Call once at startup.
func (s *Service) Recover(ctx context.Context) { s.DB.Q.FailRunningThreadMessages(ctx) }

// Ask records the question and starts the agent in the background. agentName "" picks the default.
func (s *Service) Ask(ctx context.Context, sqid, agentName, question string) (dbgen.TaskThread, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return dbgen.TaskThread{}, fmt.Errorf("%w: ask something", errs.Invalid)
	}
	t, err := s.Stack.Task(ctx, s.DB.RQ, sqid)
	if err != nil {
		return dbgen.TaskThread{}, err
	}
	ag, err := s.agent(ctx, agentName)
	if err != nil {
		return dbgen.TaskThread{}, err
	}
	session, _ := s.DB.RQ.ThreadSession(ctx, dbgen.ThreadSessionParams{TaskID: t.ID, AgentID: sql.NullInt64{Int64: ag.ID, Valid: true}})
	brief, err := s.brief(ctx, t)
	if err != nil {
		return dbgen.TaskThread{}, err
	}
	if _, err := s.DB.Q.AddThreadMessage(ctx, dbgen.AddThreadMessageParams{TaskID: t.ID, Role: "user", Body: question, Status: "done"}); err != nil {
		return dbgen.TaskThread{}, err
	}
	aid, err := s.DB.Q.AddThreadMessage(ctx, dbgen.AddThreadMessageParams{TaskID: t.ID, Role: "agent", AgentID: sql.NullInt64{Int64: ag.ID, Valid: true}, Status: "running"})
	if err != nil {
		return dbgen.TaskThread{}, err
	}
	s.Bus.Publish("thread", sqid)
	go s.answer(context.WithoutCancel(ctx), sqid, aid, ag, session, brief+"\n\n## Question\n\n"+question)
	return dbgen.TaskThread{ID: aid, TaskID: t.ID, Role: "agent", Status: "running"}, nil
}

func (s *Service) agent(ctx context.Context, name string) (dbgen.ControllerAgent, error) {
	var ag dbgen.ControllerAgent
	var err error
	if name == "" {
		ag, err = s.DB.RQ.DefaultControllerAgent(ctx)
	} else {
		ag, err = s.DB.RQ.GetControllerAgentByName(ctx, name)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ag, fmt.Errorf("%w: no controller agent %q (register one in Settings)", errs.NotFound, name)
	}
	if err == nil && ag.Enabled == 0 {
		return ag, fmt.Errorf("%w: controller agent %q is disabled", errs.Conflict, ag.Name)
	}
	return ag, err
}

func (s *Service) answer(ctx context.Context, sqid string, msgID int64, ag dbgen.ControllerAgent, session, prompt string) {
	defer s.Bus.Publish("thread", sqid)
	select { // wait for a slot, but not forever
	case s.slots() <- struct{}{}:
		defer func() { <-s.slots() }()
	case <-time.After(2 * time.Minute):
		s.finish(ctx, msgID, "Too many agents are busy on the controller; try again shortly.", "error", "")
		return
	}
	sp := Spec{Agent: ag, Prompt: prompt, System: systemPrompt, SessionID: session, Dir: s.dir()}
	if ag.Auth == "secret" {
		if s.Secret == nil || ag.AuthSecret == "" {
			s.finish(ctx, msgID, "This agent is set to use a secret but none is configured.", "error", "")
			return
		}
		v, err := s.Secret(ag.AuthSecret)
		if err != nil {
			s.finish(ctx, msgID, "Reading the agent's secret failed: "+err.Error(), "error", "")
			return
		}
		sp.Env = append(sp.Env, ag.AuthEnv+"="+strings.TrimSpace(string(v)))
	}
	run := s.Exec
	if run == nil {
		run = execCLI
	}
	timeout := time.Duration(ag.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := run(cctx, sp)
	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("timed out after %s", timeout)
		}
		s.Log.Warn("controller agent", "agent", ag.Name, "task", sqid, "err", err)
		body := "The agent failed: " + err.Error()
		if res.Text != "" {
			body = res.Text + "\n\n_" + body + "_"
		}
		s.finish(ctx, msgID, body, "error", res.SessionID)
		return
	}
	s.finish(ctx, msgID, res.Text, "done", res.SessionID)
}

func (s *Service) finish(ctx context.Context, id int64, body, status, session string) {
	if err := s.DB.Q.FinishThreadMessage(ctx, dbgen.FinishThreadMessageParams{ID: id, Body: body, Status: status, SessionID: session}); err != nil {
		s.Log.Error("finish thread message", "err", err)
	}
}

func (s *Service) dir() string {
	if s.Dir != "" {
		return s.Dir
	}
	h, _ := os.UserHomeDir()
	return h
}

// brief is the task tree under t as markdown: the focus task in full, descendants as an outline
// with their prompts, trimmed to maxContext.
func (s *Service) brief(ctx context.Context, t dbgen.Task) (string, error) {
	rows, err := s.DB.RQ.SubtreeOrdered(ctx, t.ID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Focus task %s (%s, %s)\n\n# %s\n\n%s\n", t.Sqid.String, t.TaskType, t.State, t.Title, strings.TrimSpace(t.Prompt))
	if t.CompletionSummary != "" {
		fmt.Fprintf(&b, "\nCompletion summary:\n%s\n", t.CompletionSummary)
	}
	if len(rows) > 1 {
		b.WriteString("\n## Subtasks\n")
	}
	for _, r := range rows[1:] {
		if b.Len() > maxContext {
			b.WriteString("\n…(subtree truncated; use `wrangl task show` for the rest)\n")
			break
		}
		full, err := s.DB.RQ.GetTask(ctx, r.ID)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n%s- [%s] **%s** `%s` (%s)\n", strings.Repeat("  ", int(r.Depth)-1), r.State, r.Title, r.Sqid.String, full.TaskType)
		if p := strings.TrimSpace(full.Prompt); p != "" {
			if len(p) > 1500 {
				p = p[:1500] + "…"
			}
			fmt.Fprintf(&b, "%s  > %s\n", strings.Repeat("  ", int(r.Depth)-1), strings.ReplaceAll(p, "\n", "\n"+strings.Repeat("  ", int(r.Depth)-1)+"  > "))
		}
	}
	return b.String(), nil
}

const systemPrompt = `You are a wrangl controller agent, answering the owner's question about one task and the tasks beneath it. The task tree is given below the question header; it is current as of now.

Rules:
- Be concise and concrete. Say so plainly when something is unclear or risky; the owner asked for questions and concerns, so raise them.
- Use the wrangl CLI (see the wrangl skill) to look at other tasks, runs, PRs or questions when you need more than the brief. Prefer ` + "`-o json`" + `.
- You may make small edits when the owner asks for them (edit, split, move, reprioritise, add dependencies), and say exactly what you changed. Do not delete, cancel, terminate, approve or merge anything: propose it instead.
- You run on the controller host, which must stay responsive. Do not build, compile, run tests or start long processes. Reading files is fine.`

// execCLI runs the real agent CLI.
func execCLI(ctx context.Context, sp Spec) (Result, error) {
	bin, err := exec.LookPath(sp.Agent.Command)
	if err != nil {
		return Result{}, fmt.Errorf("%q not found on the controller's PATH", sp.Agent.Command)
	}
	args := []string{"-p", sp.Prompt, "--output-format", "json",
		"--append-system-prompt", sp.System,
		"--max-turns", strconv.FormatInt(max(sp.Agent.MaxTurns, 1), 10),
		"--allowedTools", "Bash(wrangl:*)", "Read", "Grep", "Glob",
		"--disallowedTools", "Edit", "Write", "NotebookEdit", "WebFetch", "WebSearch",
		"Bash(wrangl task delete:*)", "Bash(wrangl task cancel:*)", "Bash(wrangl fleet terminate:*)", "Bash(wrangl pr approve:*)",
	}
	if sp.Agent.Model != "" {
		args = append(args, "--model", sp.Agent.Model)
	}
	if sp.SessionID != "" {
		args = append(args, "--resume", sp.SessionID)
	}
	// nice keeps a runaway agent from starving the controller.
	cmd := exec.CommandContext(ctx, "nice", append([]string{"-n", "10", bin}, args...)...)
	cmd.Dir = sp.Dir
	cmd.Env = append(os.Environ(), sp.Env...)
	if self, err := os.Executable(); err == nil { // make sure `wrangl` resolves to this build
		cmd.Env = append(cmd.Env, "PATH="+filepath.Dir(self)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	runErr := cmd.Run()
	res := parseOutput(out.Bytes())
	if runErr != nil && res.Text == "" {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 600 {
			msg = msg[:600] + "…"
		}
		if msg == "" {
			msg = runErr.Error()
		}
		return res, errors.New(msg)
	}
	if res.Text == "" {
		return res, errors.New("the agent returned nothing")
	}
	return res, nil
}

// parseOutput reads `claude -p --output-format json`. Non-JSON output is taken as the answer.
func parseOutput(b []byte) Result {
	var o struct {
		Result    string `json:"result"`
		SessionID string `json:"session_id"`
		IsError   bool   `json:"is_error"`
		Subtype   string `json:"subtype"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(b), &o); err != nil {
		return Result{Text: strings.TrimSpace(string(b))}
	}
	text := strings.TrimSpace(o.Result)
	if text == "" && o.Subtype == "error_max_turns" {
		text = "I ran out of turns before finishing; ask a narrower question or raise the agent's turn limit."
	}
	return Result{Text: text, SessionID: o.SessionID}
}

// Save registers or updates an agent by name. Setting makeDefault moves the default flag to it.
func (s *Service) Save(ctx context.Context, ag dbgen.ControllerAgent, makeDefault bool) error {
	ag.Name = strings.TrimSpace(ag.Name)
	if !regexpName.MatchString(ag.Name) {
		return fmt.Errorf("%w: agent names are lowercase letters, digits, - and _", errs.Invalid)
	}
	if ag.Kind == "" {
		ag.Kind = "claude"
	}
	if ag.Command = strings.TrimSpace(ag.Command); ag.Command == "" {
		ag.Command = "claude"
	}
	switch ag.Auth {
	case "", "cli":
		ag.Auth, ag.AuthSecret = "cli", ""
	case "secret":
		if ag.AuthSecret == "" {
			return fmt.Errorf("%w: auth \"secret\" needs a secret name", errs.Invalid)
		}
		if ag.AuthEnv == "" {
			ag.AuthEnv = "CLAUDE_CODE_OAUTH_TOKEN"
		}
	default:
		return fmt.Errorf("%w: auth must be cli or secret", errs.Invalid)
	}
	if ag.AuthEnv == "" {
		ag.AuthEnv = "CLAUDE_CODE_OAUTH_TOKEN"
	}
	if ag.MaxTurns <= 0 {
		ag.MaxTurns = 12
	}
	if ag.TimeoutS <= 0 {
		ag.TimeoutS = 300
	}
	return s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		if _, err := q.UpsertControllerAgent(ctx, dbgen.UpsertControllerAgentParams{Name: ag.Name, Kind: ag.Kind, Command: ag.Command,
			Model: ag.Model, Auth: ag.Auth, AuthSecret: ag.AuthSecret, AuthEnv: ag.AuthEnv, MaxTurns: ag.MaxTurns, TimeoutS: ag.TimeoutS, Enabled: ag.Enabled}); err != nil {
			return err
		}
		if makeDefault {
			if err := q.ClearDefaultControllerAgent(ctx); err != nil {
				return err
			}
			return q.SetDefaultControllerAgent(ctx, ag.Name)
		}
		return nil
	})
}

// Delete removes an agent; the last one cannot be removed (Ask needs a default).
func (s *Service) Delete(ctx context.Context, name string) error {
	all, err := s.DB.RQ.ListControllerAgents(ctx)
	if err != nil {
		return err
	}
	if len(all) <= 1 {
		return fmt.Errorf("%w: keep at least one controller agent", errs.Conflict)
	}
	wasDefault := false
	for _, a := range all {
		if a.Name == name {
			wasDefault = a.IsDefault == 1
		}
	}
	if err := s.DB.Q.DeleteControllerAgent(ctx, name); err != nil {
		return err
	}
	if wasDefault {
		if rest, _ := s.DB.RQ.ListControllerAgents(ctx); len(rest) > 0 {
			s.DB.Q.SetDefaultControllerAgent(ctx, rest[0].Name)
		}
	}
	return nil
}

var regexpName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

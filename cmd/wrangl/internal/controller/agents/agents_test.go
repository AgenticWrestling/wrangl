package agents

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/events"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

func setup(t *testing.T) (*Service, string, *[]Spec) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	d.W.Exec(`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('go','','{}',1,1,1)`)
	d.W.Exec(`INSERT INTO project (slug, name, default_template_id) VALUES ('p','P',(SELECT id FROM runner_template))`)
	sq, _ := stack.NewSqids(stack.DefaultSqidsAlphabet)
	st := &stack.Service{DB: d, Sqids: sq}
	root, err := st.Create(ctx, stack.CreateParams{Project: "p", Title: "Root", Prompt: "do the thing", Actor: stack.User})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, stack.CreateParams{Project: "p", Parent: root.Sqid.String, Title: "Child A", Prompt: "first part", Actor: stack.User}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []Spec
	s := &Service{DB: d, Stack: st, Bus: &events.Bus{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Secret: func(n string) ([]byte, error) { return []byte(" tok-" + n + "\n"), nil },
		Exec: func(_ context.Context, sp Spec) (Result, error) {
			mu.Lock()
			seen = append(seen, sp)
			mu.Unlock()
			return Result{Text: "Looks fine. **One concern**: Child A is vague.", SessionID: "sess-1"}, nil
		}}
	return s, root.Sqid.String, &seen
}

func waitDone(t *testing.T, s *Service, sqid string) []dbgen.TaskThread {
	t.Helper()
	tk, _ := s.Stack.Task(context.Background(), s.DB.RQ, sqid)
	for i := 0; i < 200; i++ {
		rows, _ := s.DB.RQ.ListThread(context.Background(), tk.ID)
		if len(rows) > 0 && rows[len(rows)-1].Status != "running" {
			return rows
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("agent never finished")
	return nil
}

func TestAskThreadAndResume(t *testing.T) {
	s, root, seen := setup(t)
	if _, err := s.Ask(context.Background(), root, "", "Any concerns?"); err != nil {
		t.Fatal(err)
	}
	rows := waitDone(t, s, root)
	if len(rows) != 2 || rows[0].Role != "user" || rows[1].Role != "agent" || rows[1].Status != "done" || rows[1].SessionID != "sess-1" || !strings.Contains(rows[1].Body, "One concern") {
		t.Fatalf("%+v", rows)
	}
	first := (*seen)[0]
	for _, want := range []string{"Root", "do the thing", "Child A", "first part", "Any concerns?"} {
		if !strings.Contains(first.Prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, first.Prompt)
		}
	}
	if first.SessionID != "" || first.Env != nil {
		t.Fatalf("first ask has no session or env: %+v", first)
	}
	// a follow-up resumes the CLI session
	if _, err := s.Ask(context.Background(), root, "claude", "And the second part?"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200 && len(*seen) < 2; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if got := (*seen)[1].SessionID; got != "sess-1" {
		t.Fatalf("resume %q", got)
	}
}

func TestAskFailureAndValidation(t *testing.T) {
	s, root, _ := setup(t)
	s.Exec = func(context.Context, Spec) (Result, error) { return Result{}, errors.New("boom") }
	if _, err := s.Ask(context.Background(), root, "", "  "); !errors.Is(err, errs.Invalid) {
		t.Fatalf("blank: %v", err)
	}
	if _, err := s.Ask(context.Background(), "nope", "", "q"); !errors.Is(err, errs.NotFound) {
		t.Fatalf("task: %v", err)
	}
	if _, err := s.Ask(context.Background(), root, "ghost", "q"); !errors.Is(err, errs.NotFound) {
		t.Fatalf("agent: %v", err)
	}
	s.Ask(context.Background(), root, "", "q")
	rows := waitDone(t, s, root)
	if rows[1].Status != "error" || !strings.Contains(rows[1].Body, "boom") {
		t.Fatalf("%+v", rows[1])
	}
}

func TestSecretAuthAndRegistry(t *testing.T) {
	s, root, seen := setup(t)
	ctx := context.Background()
	if err := s.Save(ctx, dbgen.ControllerAgent{Name: "tok", Auth: "secret", Enabled: 1}, true); !errors.Is(err, errs.Invalid) {
		t.Fatalf("secret needs a name: %v", err)
	}
	if err := s.Save(ctx, dbgen.ControllerAgent{Name: "Bad Name", Enabled: 1}, false); !errors.Is(err, errs.Invalid) {
		t.Fatalf("name: %v", err)
	}
	if err := s.Save(ctx, dbgen.ControllerAgent{Name: "tok", Auth: "secret", AuthSecret: "claude-token", Enabled: 1}, true); err != nil {
		t.Fatal(err)
	}
	def, _ := s.DB.RQ.DefaultControllerAgent(ctx)
	if def.Name != "tok" || def.AuthEnv != "CLAUDE_CODE_OAUTH_TOKEN" || def.MaxTurns != 12 {
		t.Fatalf("%+v", def)
	}
	s.Ask(ctx, root, "", "q")
	waitDone(t, s, root)
	if env := (*seen)[0].Env; len(env) != 1 || env[0] != "CLAUDE_CODE_OAUTH_TOKEN=tok-claude-token" {
		t.Fatalf("env %v", env)
	}
	if err := s.Delete(ctx, "tok"); err != nil {
		t.Fatal(err)
	}
	if def, _ := s.DB.RQ.DefaultControllerAgent(ctx); def.Name != "claude" {
		t.Fatalf("default falls back: %+v", def)
	}
	if err := s.Delete(ctx, "claude"); !errors.Is(err, errs.Conflict) {
		t.Fatalf("last agent: %v", err)
	}
}

func TestParseOutput(t *testing.T) {
	r := parseOutput([]byte(`{"type":"result","result":" hi ","session_id":"s9","is_error":false}`))
	if r.Text != "hi" || r.SessionID != "s9" {
		t.Fatalf("%+v", r)
	}
	if r := parseOutput([]byte(`{"subtype":"error_max_turns","result":""}`)); !strings.Contains(r.Text, "turns") {
		t.Fatalf("%+v", r)
	}
	if r := parseOutput([]byte("plain text\n")); r.Text != "plain text" {
		t.Fatalf("%+v", r)
	}
}

func TestExtractProposals(t *testing.T) {
	in := "Two ideas.\n\n```wrangl-proposals\n[{\"label\":\"Add\",\"action\":\"task.create\",\"args\":{\"title\":\"x\"}},{\"label\":\"Nuke\",\"action\":\"task.delete\",\"args\":{}},{\"label\":\"\",\"action\":\"task.edit\"},{\"label\":\"Q\",\"action\":\"task.queue\",\"args\":{\"sqid\":\"a\"}}]\n```\n"
	text, ps := extractProposals(in)
	if text != "Two ideas." || len(ps) != 2 || ps[0].Action != "task.create" || ps[1].Action != "task.queue" {
		t.Fatalf("%q %+v", text, ps)
	}
	if text, ps := extractProposals("hi\n```wrangl-proposals\nnot json\n```"); text != "hi" || ps != nil {
		t.Fatalf("%q %+v", text, ps)
	}
	var many []string
	for i := 0; i < 8; i++ {
		many = append(many, `{"label":"l","action":"task.reopen","args":{"sqid":"a"}}`)
	}
	if _, ps := extractProposals("```wrangl-proposals\n[" + strings.Join(many, ",") + "]\n```"); len(ps) != MaxProposals {
		t.Fatalf("cap: %d", len(ps))
	}
}

func TestProposalApply(t *testing.T) {
	s, root, _ := setup(t)
	ctx := context.Background()
	var queued string
	s.Queue = func(_ context.Context, sqid string) error { queued = sqid; return nil }
	s.Exec = func(context.Context, Spec) (Result, error) {
		return Result{Text: "Try these.\n```wrangl-proposals\n[{\"label\":\"New sibling\",\"action\":\"task.create\",\"args\":{\"title\":\"Child B\",\"parent\":\"" + root + "\"}},{\"label\":\"Queue it\",\"action\":\"task.queue\",\"args\":{\"sqid\":\"" + root + "\"}},{\"label\":\"Bad\",\"action\":\"task.move\",\"args\":{\"sqid\":\"zzzz\"}}]\n```"}, nil
	}
	s.Ask(ctx, root, "", "ideas?")
	rows := waitDone(t, s, root)
	msg := rows[1]
	if len(ParseProposals(msg.Proposals)) != 3 || strings.Contains(msg.Body, "wrangl-proposals") {
		t.Fatalf("%+v", msg)
	}
	if err := s.Apply(ctx, msg.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, msg.ID, 0); !errors.Is(err, errs.Conflict) {
		t.Fatalf("single use: %v", err)
	}
	if err := s.Apply(ctx, msg.ID, 1); err != nil || queued != root {
		t.Fatalf("queue: %v %q", err, queued)
	}
	if err := s.Apply(ctx, msg.ID, 2); err == nil {
		t.Fatal("moving a missing task must fail")
	}
	got := ParseProposals(func() string { m, _ := s.DB.RQ.GetThreadMessage(ctx, msg.ID); return m.Proposals }())
	if got[0].State != "done" || !strings.HasPrefix(got[0].Result, "created ") || got[2].State != "error" {
		t.Fatalf("%+v", got)
	}
	tk, _ := s.Stack.Task(ctx, s.DB.RQ, root)
	kids, _ := s.DB.RQ.ListChildren(ctx, sql.NullInt64{Int64: tk.ID, Valid: true})
	if len(kids) != 2 {
		t.Fatalf("children %d", len(kids))
	}
	if err := s.Apply(ctx, msg.ID, 9); !errors.Is(err, errs.NotFound) {
		t.Fatalf("range: %v", err)
	}
}

func TestProposalGoesStale(t *testing.T) {
	s, root, _ := setup(t)
	ctx := context.Background()
	s.Exec = func(context.Context, Spec) (Result, error) {
		return Result{Text: "x\n```wrangl-proposals\n[{\"label\":\"Reopen\",\"action\":\"task.reopen\",\"args\":{\"sqid\":\"" + root + "\"}}]\n```"}, nil
	}
	s.Ask(ctx, root, "", "q")
	msg := waitDone(t, s, root)[1]
	time.Sleep(20 * time.Millisecond) // the rev stamp follows the finish
	if _, err := s.Stack.Create(ctx, stack.CreateParams{Project: "p", Parent: root, Title: "Surprise", Actor: stack.User}); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, msg.ID, 0); !errors.Is(err, errs.Conflict) || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("stale: %v", err)
	}
}

// fleet.launch reaches the launcher with the task's project, the template and the count, and an
// error from it (the cap refusal) is shown on the proposal.
func TestFleetLaunchProposal(t *testing.T) {
	s, root, _ := setup(t)
	ctx := context.Background()
	var got string
	s.Launch = func(_ context.Context, project, template string, n int) ([]string, error) {
		got = fmt.Sprintf("%s/%s/%d", project, template, n)
		if template == "mac" {
			return nil, fmt.Errorf("%w: project p may run at most 0 instance(s) of template mac (project limit 0, template max_runners 2)", errs.Conflict)
		}
		return []string{"r1"}, nil
	}
	s.Exec = func(context.Context, Spec) (Result, error) {
		return Result{Text: "ok\n```wrangl-proposals\n[{\"label\":\"Start Android\",\"action\":\"fleet.launch\",\"args\":{\"template\":\"android\",\"count\":2}},{\"label\":\"Start Mac\",\"action\":\"fleet.launch\",\"args\":{\"template\":\"mac\"}}]\n```"}, nil
	}
	s.Ask(ctx, root, "", "need a device")
	msg := waitDone(t, s, root)[1]
	if err := s.Apply(ctx, msg.ID, 0); err != nil || got != "p/android/2" {
		t.Fatalf("launch: %v %q", err, got)
	}
	err := s.Apply(ctx, msg.ID, 1)
	if err == nil || !strings.Contains(err.Error(), "project limit 0") {
		t.Fatalf("refusal must carry the cap: %v", err)
	}
	if !strings.Contains(schemaHelp(), "fleet.launch") {
		t.Fatal("system prompt must document fleet.launch")
	}
}

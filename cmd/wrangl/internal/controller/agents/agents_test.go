package agents

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nreid/devhost/cmd/wrangl/internal/db"
	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbgen"
	"github.com/nreid/devhost/cmd/wrangl/internal/errs"
	"github.com/nreid/devhost/cmd/wrangl/internal/events"
	"github.com/nreid/devhost/cmd/wrangl/internal/stack"
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

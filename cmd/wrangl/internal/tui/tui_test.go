package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/client"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

type fakeAPI struct {
	calls []string
	tree  []proto.TaskNode
}

func (f *fakeAPI) rec(s string, a ...any) error {
	f.calls = append(f.calls, fmt.Sprintf(s, a...))
	return nil
}
func (f *fakeAPI) Runners(context.Context, bool) ([]proto.RunnerView, error) {
	return []proto.RunnerView{{Name: "go-1", State: "idle", Template: "go", Metrics: json.RawMessage(`{"cpu_percent":12}`)}}, nil
}
func (f *fakeAPI) Runs(context.Context, string, string, string, int) ([]proto.RunView, error) {
	return []proto.RunView{{ID: 7, State: "running", Kind: "work", Task: "aaa111"}}, nil
}
func (f *fakeAPI) Projects(context.Context) ([]proto.ProjectView, error) {
	return []proto.ProjectView{{Slug: "web"}}, nil
}
func (f *fakeAPI) ProjectTree(context.Context, string, bool) ([]proto.TaskNode, error) {
	return f.tree, nil
}
func (f *fakeAPI) Questions(context.Context, bool) ([]proto.QuestionView, error) {
	return []proto.QuestionView{{ID: 3, Kind: "agent", Prompt: "Which colour?", Options: []proto.QuestionOption{{Label: "Red", Value: "red"}, {Label: "Blue", Value: "blue"}}}}, nil
}
func (f *fakeAPI) PRs(context.Context, string) ([]proto.PRView, error) {
	return []proto.PRView{{ID: 9, Number: 4, Repo: "acme/app"}}, nil
}
func (f *fakeAPI) CreateTask(_ context.Context, r proto.TaskCreateRequest) (proto.TaskRef, error) {
	return proto.TaskRef{}, f.rec("create parent=%s after=%s title=%s", r.Parent, r.After, r.Title)
}
func (f *fakeAPI) MoveTask(_ context.Context, sqid string, r proto.TaskMoveRequest) (proto.TaskRef, error) {
	p := "-"
	if r.Parent != nil {
		p = *r.Parent
	}
	return proto.TaskRef{}, f.rec("move %s parent=%s root=%v after=%s before=%s", sqid, p, r.Root, r.After, r.Before)
}
func (f *fakeAPI) NeedsAdd(_ context.Context, a, b string) error { return f.rec("needs %s %s", a, b) }
func (f *fakeAPI) QueueTask(_ context.Context, s string) (int64, string, error) {
	return 1, "", f.rec("queue %s", s)
}
func (f *fakeAPI) TaskAction(_ context.Context, s, op, _ string) error { return f.rec("%s %s", op, s) }
func (f *fakeAPI) SplitTask(_ context.Context, s string, r proto.TaskSplitRequest) ([]proto.TaskRef, error) {
	return nil, f.rec("split %s %d", s, len(r.Children))
}
func (f *fakeAPI) Answer(_ context.Context, id int64, v, t string) error {
	return f.rec("answer %d %s %s", id, v, t)
}
func (f *fakeAPI) PRAction(_ context.Context, id int64, op, m string) error {
	return f.rec("pr %d %s %s", id, op, m)
}
func (f *fakeAPI) CancelRun(_ context.Context, id int64) error { return f.rec("cancel run %d", id) }
func (f *fakeAPI) RunnerAction(_ context.Context, n, a string) error {
	return f.rec("runner %s %s", n, a)
}
func (f *fakeAPI) Terminate(_ context.Context, n string) error { return f.rec("terminate %s", n) }
func (f *fakeAPI) Events(ctx context.Context, _ func(client.Event)) error {
	<-ctx.Done()
	return nil
}

// drive feeds keys and runs the resulting commands synchronously (one level deep, which
// is enough for action -> statusMsg).
func drive(t *testing.T, m Model, keys ...string) Model {
	t.Helper()
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		nm, cmd := m.Update(msg)
		m = nm.(Model)
		if cmd != nil {
			if out := cmd(); out != nil {
				if _, isQuit := out.(tea.QuitMsg); !isQuit {
					nm, _ := m.Update(out)
					m = nm.(Model)
				}
			}
		}
	}
	return m
}

func loaded(t *testing.T, api *fakeAPI) Model {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var execd []string
	m := New(ctx, api, func(args ...string) tea.Cmd { execd = append(execd, strings.Join(args, " ")); return nil })
	nm, _ := m.Update(m.load()())
	return nm.(Model)
}

func TestPanesRender(t *testing.T) {
	m := loaded(t, &fakeAPI{tree: []proto.TaskNode{{Sqid: "aaa111", Title: "epic", State: "open"}}})
	out := m.View()
	for _, want := range []string{"1 Runners (1)", "4 Questions (1!)", "go-1", "cpu  12%"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	m = drive(t, m, "3")
	if !strings.Contains(m.View(), "aaa111 ○ epic") {
		t.Fatalf("stack pane:\n%s", m.View())
	}
}

func TestStackEditing(t *testing.T) {
	api := &fakeAPI{tree: []proto.TaskNode{
		{Sqid: "epic01", Title: "epic", State: "open", Depth: 0},
		{Sqid: "kid001", Parent: "epic01", Title: "a", State: "open", Depth: 1},
		{Sqid: "kid002", Parent: "epic01", Title: "b", State: "open", Depth: 1},
	}}
	m := drive(t, loaded(t, api), "3")
	m = drive(t, m, "n", "w", "r", "i", "t", "e", "enter") // subtask of epic01
	m = drive(t, m, "j", "J")                              // kid001 after kid002
	m = drive(t, m, "j", "K")                              // kid002 before kid001
	m = drive(t, m, ">")                                   // kid002 under kid001
	m = drive(t, m, "<")                                   // kid002 out to top level after epic01
	m = drive(t, m, "Q", "x")
	m = drive(t, m, "s", "a", ";", "b", "enter")
	m = drive(t, m, "c", "y")
	want := []string{
		"create parent=epic01 after= title=write",
		"move kid001 parent=- root=false after=kid002 before=",
		"move kid002 parent=- root=false after= before=kid001",
		"move kid002 parent=kid001 root=false after= before=",
		"move kid002 parent=- root=true after=epic01 before=",
		"queue kid002", "complete kid002", "split kid002 2", "cancel kid002",
	}
	if got := strings.Join(api.calls, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
}

func TestAnswersAndPRs(t *testing.T) {
	api := &fakeAPI{}
	m := drive(t, loaded(t, api), "tab", "tab", "tab") // questions
	m = drive(t, m, "2")
	m = drive(t, m, "tab")    // PRs
	m = drive(t, m, "A", "n") // declined confirmation: no call
	m = drive(t, m, "A", "y")
	m = drive(t, m, "R", "f", "i", "x", "enter")
	want := "answer 3 blue \npr 9 approve \npr 9 request-changes fix"
	if got := strings.Join(api.calls, "\n"); got != want {
		t.Fatalf("calls:\n%q\nwant\n%q", got, want)
	}
}

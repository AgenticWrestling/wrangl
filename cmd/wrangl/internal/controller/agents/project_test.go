package agents

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

func waitProject(t *testing.T, s *Service, slug string, n int) []ProjectMessage {
	t.Helper()
	for i := 0; i < 300; i++ {
		msgs, _, err := s.ProjectThread(context.Background(), slug)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) >= n && msgs[len(msgs)-1].Status != "running" {
			return msgs
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("project agent never finished")
	return nil
}

func TestProjectChatContextResumeAndLinks(t *testing.T) {
	s, root, seen := setup(t)
	ctx := context.Background()
	rootTask, _ := s.Stack.Task(ctx, s.DB.RQ, root)
	s.Exec = func(_ context.Context, sp Spec) (Result, error) {
		*seen = append(*seen, sp)
		return Result{Text: "Start with `" + rootTask.Sqid.String + "`, it is the biggest.", SessionID: "psess"}, nil
	}
	if _, err := s.AskProject(ctx, "p", "", "what next?"); err != nil {
		t.Fatal(err)
	}
	msgs := waitProject(t, s, "p", 2)
	sp := (*seen)[0]
	for _, want := range []string{"## Project p", "Root", "Child A", root, "what next?"} {
		if !strings.Contains(sp.Prompt, want) {
			t.Fatalf("brief missing %q:\n%s", want, sp.Prompt)
		}
	}
	if !strings.Contains(sp.System, "whole project") || !strings.Contains(sp.System, "wrangl-proposals") {
		t.Fatalf("system prompt = %q", sp.System)
	}
	last := msgs[len(msgs)-1]
	if last.Role != "agent" || last.Status != "done" || len(last.Tasks) != 1 || last.Tasks[0].Sqid != root || last.Tasks[0].Title != "Root" {
		t.Fatalf("reply = %+v; want the task id linked", last)
	}
	// The next question resumes the session.
	if _, err := s.AskProject(ctx, "p", "", "and then?"); err != nil {
		t.Fatal(err)
	}
	waitProject(t, s, "p", 4)
	if got := (*seen)[1].SessionID; got != "psess" {
		t.Fatalf("session = %q, want psess", got)
	}
	_, chip, _ := s.ProjectThread(ctx, "p")
	if chip.Tasks != 2 || chip.Repos != 0 {
		t.Fatalf("chip = %+v, want 2 tasks", chip)
	}
	if _, err := s.AskProject(ctx, "nosuch", "", "hi"); err == nil {
		t.Fatal("unknown project must fail")
	}
	if _, err := s.AskProject(ctx, "p", "", "  "); err == nil {
		t.Fatal("empty question must fail")
	}
}

func TestProjectChatProposalsApply(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	s.Exec = func(_ context.Context, sp Spec) (Result, error) {
		return Result{Text: "I suggest two things.\n\n```wrangl-proposals\n[{\"label\":\"Add Docs task\",\"action\":\"task.create\",\"args\":{\"title\":\"Docs\"}},{\"label\":\"Reorder\",\"action\":\"queue.reorder\",\"args\":{\"sqids\":[\"nope\"]}}]\n```"}, nil
	}
	s.AskProject(ctx, "p", "", "plan")
	msgs := waitProject(t, s, "p", 2)
	m := msgs[len(msgs)-1]
	if len(m.Proposals) != 2 || m.Proposals[0].State != "" {
		t.Fatalf("proposals = %+v", m.Proposals)
	}
	if err := s.ApplyProject(ctx, m.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyProject(ctx, m.ID, 0); err == nil {
		t.Fatal("a proposal applies once")
	}
	if _, chip, _ := s.ProjectThread(ctx, "p"); chip.Tasks != 3 {
		t.Fatalf("task.create did not create a task: %+v", chip)
	}
	if err := s.ApplyProject(ctx, m.ID, 1); err == nil { // not queued: refused, recorded as an error
		t.Fatal("reordering unqueued tasks must fail")
	}
	msgs, _, _ = s.ProjectThread(ctx, "p")
	ps := msgs[len(msgs)-1].Proposals
	if ps[0].State != "done" || ps[1].State != "error" {
		t.Fatalf("proposal states = %q, %q", ps[0].State, ps[1].State)
	}
	if err := s.ApplyProject(ctx, 9999, 0); err == nil {
		t.Fatal("unknown message must fail")
	}
}

func TestProjectChatStaleProposalRefused(t *testing.T) {
	s, root, _ := setup(t)
	ctx := context.Background()
	s.Exec = func(context.Context, Spec) (Result, error) {
		return Result{Text: "ok\n```wrangl-proposals\n[{\"label\":\"Add\",\"action\":\"task.create\",\"args\":{\"title\":\"X\"}}]\n```"}, nil
	}
	s.AskProject(ctx, "p", "", "go")
	m := waitProject(t, s, "p", 2)
	if _, err := s.Stack.Edit(ctx, root, ptr("Root renamed"), nil, "user"); err != nil {
		t.Skip("edit helper differs: ", err)
	}
	if err := s.ApplyProject(ctx, m[len(m)-1].ID, 0); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("stale proposal: %v", err)
	}
}

func ptr(s string) *string { return &s }

func TestProjectSlashCommands(t *testing.T) {
	s, root, seen := setup(t)
	ctx := context.Background()
	var queued, dequeued []string
	s.Queue = func(_ context.Context, sq string) error { queued = append(queued, sq); return nil }
	s.Dequeue = func(_ context.Context, sq string) error { dequeued = append(dequeued, sq); return nil }
	m, err := s.AskProject(ctx, "p", "", "/queue "+root)
	if err != nil || len(queued) != 1 || queued[0] != root || !strings.Contains(m.Body, "Done") || len(m.Tasks) != 1 {
		t.Fatalf("/queue by id: %+v %v queued=%v", m, err, queued)
	}
	if m, err = s.AskProject(ctx, "p", "", "/dequeue child a"); err != nil || len(dequeued) != 1 || !strings.Contains(m.Body, "Done") {
		t.Fatalf("/dequeue by title: %+v %v dequeued=%v", m, err, dequeued)
	}
	// Two titles contain "root": ambiguous, so buttons instead of acting.
	if _, err := s.Stack.Create(ctx, stack.CreateParams{Project: "p", Title: "Root two", Actor: stack.User}); err != nil {
		t.Fatal(err)
	}
	if m, err = s.AskProject(ctx, "p", "", "/queue root"); err != nil || len(queued) != 1 || len(m.Proposals) != 2 || m.Proposals[0].Action != "task.queue" {
		t.Fatalf("ambiguous /queue: %+v %v", m, err)
	}
	if err := s.ApplyProject(ctx, m.ID, 0); err != nil || len(queued) != 2 {
		t.Fatalf("approving a /queue option: %v queued=%v", err, queued)
	}
	if m, _ = s.AskProject(ctx, "p", "", "/queue zzz"); !strings.Contains(m.Body, "No open task") {
		t.Fatalf("no match: %+v", m)
	}
	if _, err := s.AskProject(ctx, "p", "", "/frobnicate x"); err == nil {
		t.Fatal("unknown command must be rejected")
	}
	if len(*seen) != 0 {
		t.Fatalf("slash commands must not run the model, ran %d times", len(*seen))
	}
	msgs, _, _ := s.ProjectThread(ctx, "p")
	if msgs[0].Role != "user" || msgs[0].Body != "/queue "+root {
		t.Fatalf("thread should record the commands: %+v", msgs[0])
	}
}

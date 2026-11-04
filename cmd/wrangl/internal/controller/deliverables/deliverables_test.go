package deliverables_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/deliverables"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/events"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

var png = []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 32))

func setup(t *testing.T) (*deliverables.Service, runs.Scope) {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, q := range []string{
		`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('go','','{}',1,1,1)`,
		`INSERT INTO project (slug, name) VALUES ('p','p')`,
		`INSERT INTO task (project_id, title, rank) VALUES (1, 'design it', 'A')`,
		`INSERT INTO run (task_id) VALUES (1)`,
	} {
		if _, err := d.W.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return &deliverables.Service{DB: d, Dir: filepath.Join(t.TempDir(), "dl"), Log: slog.Default()}, runs.Scope{RunID: 1, Kind: "work"}
}

func call(t *testing.T, s *deliverables.Service, sc runs.Scope, v map[string]any) error {
	t.Helper()
	b, _ := json.Marshal(v)
	_, err := s.Submit(context.Background(), sc, b)
	return err
}

func TestSubmitKindsAndOpen(t *testing.T) {
	s, sc := setup(t)
	ctx := context.Background()
	if err := call(t, s, sc, map[string]any{"kind": "image", "title": "home, phone", "image_base64": base64.StdEncoding.EncodeToString(png)}); err != nil {
		t.Fatal(err)
	}
	if err := call(t, s, sc, map[string]any{"kind": "artifact_url", "title": "prototype", "url": "https://claude.ai/artifact/abc"}); err != nil {
		t.Fatal(err)
	}
	if err := call(t, s, sc, map[string]any{"kind": "report_md", "title": "memo", "content_md": "# Answer\n\nyes [1]"}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.DB.RQ.ListDeliverablesForTask(ctx, 1)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows %d %v", len(rows), err)
	}
	d, path, err := s.Open(ctx, rows[0].ID)
	if err != nil || d.Mime != "image/png" || path == "" {
		t.Fatalf("open image: %+v %q %v", d, path, err)
	}
	if b, _ := os.ReadFile(path); string(b) != string(png) {
		t.Fatal("stored bytes differ")
	}
	if _, p, _ := s.Open(ctx, rows[1].ID); p != "" {
		t.Fatalf("a URL deliverable has no file, got %q", p)
	}
	if _, _, err := s.Open(ctx, 999); !errors.Is(err, errs.NotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestSubmitRejects(t *testing.T) {
	s, sc := setup(t)
	for name, v := range map[string]map[string]any{
		"not an image":     {"kind": "image", "image_base64": base64.StdEncoding.EncodeToString([]byte("<html><script>alert(1)</script>"))},
		"bad base64":       {"kind": "image", "image_base64": "!!!"},
		"javascript url":   {"kind": "link", "url": "javascript:alert(1)"},
		"no url":           {"kind": "artifact_url"},
		"empty report":     {"kind": "report_md", "content_md": "  "},
		"unknown kind":     {"kind": "video"},
		"oversized report": {"kind": "report_md", "content_md": strings.Repeat("x", deliverables.MaxReport+1)},
	} {
		if err := call(t, s, sc, v); !errors.Is(err, errs.Invalid) {
			t.Errorf("%s: want invalid, got %v", name, err)
		}
	}
	if err := call(t, s, runs.Scope{}, map[string]any{"kind": "link", "url": "https://x.example"}); err == nil {
		t.Error("the owner scope (no run) must be refused")
	}
}

func TestSweepRemovesOldOnly(t *testing.T) {
	s, sc := setup(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := call(t, s, sc, map[string]any{"kind": "image", "image_base64": base64.StdEncoding.EncodeToString(png)}); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-100 * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	if _, err := s.DB.W.Exec(`UPDATE deliverable SET created_at = ? WHERE id = 1`, old); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.DB.RQ.ListDeliverablesForTask(ctx, 1)
	oldPath := filepath.Join(s.Dir, rows[0].File)
	n, err := s.Sweep(ctx, deliverables.Retention)
	if err != nil || n != 1 {
		t.Fatalf("swept %d %v", n, err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("old file not removed")
	}
	if rows, _ := s.DB.RQ.ListDeliverablesForTask(ctx, 1); len(rows) != 1 {
		t.Fatalf("remaining %d", len(rows))
	}
}

func TestDeliverableReviewFlow(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, q := range []string{
		`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('go','','{}',1,1,1)`,
		`INSERT INTO project (slug, name, default_template_id) VALUES ('p','p',1)`,
		`INSERT INTO repo (project_id, owner, name) VALUES (1,'o','a')`,
	} {
		if _, err := d.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	sq, _ := stack.NewSqids(stack.DefaultSqidsAlphabet)
	st := &stack.Service{DB: d, Sqids: sq, Bus: &events.Bus{}}
	task, err := st.Create(ctx, stack.CreateParams{Project: "p", Title: "design the home screen", Type: "ux", Actor: stack.User})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.Queue(ctx, task.Sqid.String)
	if err != nil {
		t.Fatal(err)
	}
	// The agent finished: the task awaits acceptance and nothing was pushed.
	if _, err := d.W.Exec(`UPDATE task SET state='in_review', completed_run_id=? WHERE id=?`, run.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`UPDATE run SET state='succeeded', pushed=0 WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	rs := &runs.Service{DB: d, Stack: st, Log: slog.Default(), Bus: &events.Bus{}, Now: time.Now, AnswerHandlers: map[string]runs.AnswerHandler{}}
	s := &deliverables.Service{DB: d, Dir: t.TempDir(), Log: slog.Default(), Bus: &events.Bus{}}
	rv := deliverables.Review{Runs: rs, Stack: st}
	rs.AnswerHandlers["deliverable_review"] = s.Answer(rv)

	// No deliverable: no question.
	s.OnRunFinished(ctx, rv, run.ID)
	if qs, _ := rs.Questions(ctx, false); len(qs) != 0 {
		t.Fatalf("question without deliverables: %d", len(qs))
	}
	if err := call(t, s, runs.Scope{RunID: run.ID, Kind: "work"}, map[string]any{"kind": "artifact_url", "title": "prototype", "url": "https://claude.ai/artifact/x"}); err != nil {
		t.Fatal(err)
	}
	s.OnRunFinished(ctx, rv, run.ID)
	s.OnRunFinished(ctx, rv, run.ID) // idempotent
	qs, _ := rs.Questions(ctx, false)
	if len(qs) != 1 || qs[0].Kind != "deliverable_review" {
		t.Fatalf("questions: %+v", qs)
	}
	if err := rs.Answer(ctx, qs[0].ID, "request_changes", ""); err == nil {
		t.Fatal("request_changes without text must be refused")
	}

	q := dbgen.Question{TaskID: task.ID, PayloadJson: fmt.Sprintf(`{"run_id":%d}`, run.ID)}
	h := s.Answer(rv)
	state := func() (string, string) {
		cur, _ := st.Task(ctx, d.RQ, task.Sqid.String)
		return cur.State, cur.Prompt
	}
	// Request changes: back to open with the feedback in the prompt, and a new run queued.
	if err := h(ctx, q, "request_changes", "make the header smaller"); err != nil {
		t.Fatal(err)
	}
	if st8, prompt := state(); st8 != "in_progress" && st8 != "open" || !strings.Contains(prompt, "make the header smaller") {
		t.Fatalf("after request_changes: %s %q", st8, prompt)
	}
	if n, _ := d.RQ.CountRunsForTask(ctx, task.ID); n != 2 {
		t.Fatalf("runs after request_changes: %d", n)
	}
	// Approve: the tasks the run completed become done.
	if _, err := d.W.Exec(`UPDATE task SET state='in_review', completed_run_id=? WHERE id=?`, run.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := h(ctx, q, "approve", ""); err != nil {
		t.Fatal(err)
	}
	if st8, _ := state(); st8 != "done" {
		t.Fatalf("after approve: %s", st8)
	}
}

package stack_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

func setup(t *testing.T) (*stack.Service, *db.DB) {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	sq, _ := stack.NewSqids(stack.DefaultSqidsAlphabet)
	for _, q := range []string{
		`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('go','','{}',1,1,1)`,
		`INSERT INTO project (slug, name, default_template_id) VALUES ('p', 'P', 1), ('q', 'Q', 1)`,
		`INSERT INTO repo (project_id, owner, name) VALUES (1, 'o', 'a'), (1, 'o', 'b')`,
	} {
		if _, err := d.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return &stack.Service{DB: d, Sqids: sq}, d
}

func mk(t *testing.T, s *stack.Service, parent, title string, pos stack.Position) dbgen.Task {
	t.Helper()
	tk, err := s.Create(context.Background(), stack.CreateParams{Project: "p", Parent: parent, Title: title, Pos: pos, Actor: stack.User})
	if err != nil {
		t.Fatalf("create %s: %v", title, err)
	}
	return tk
}

func titles(t *testing.T, d *db.DB, root int64) string {
	t.Helper()
	rows, err := d.RQ.SubtreeOrdered(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows[1:] {
		out = append(out, r.Title)
	}
	return strings.Join(out, ",")
}

func state(t *testing.T, s *stack.Service, d *db.DB, sqid string) string {
	t.Helper()
	tk, err := s.Task(context.Background(), d.RQ, sqid)
	if err != nil {
		t.Fatal(err)
	}
	return tk.State
}

func TestPositionsAndMove(t *testing.T) {
	s, d := setup(t)
	ctx := context.Background()
	epic := mk(t, s, "", "epic", stack.Position{})
	b := mk(t, s, epic.Sqid.String, "b", stack.Position{})
	mk(t, s, epic.Sqid.String, "d", stack.Position{})
	mk(t, s, epic.Sqid.String, "a", stack.Position{First: true})
	mk(t, s, epic.Sqid.String, "c", stack.Position{After: b.Sqid.String})
	if got := titles(t, d, epic.ID); got != "a,b,c,d" {
		t.Fatalf("order %s", got)
	}
	// Move d before a; then make b a child of a.
	dTask, _ := d.RQ.ListChildren(ctx, sqlNull(epic.ID))
	dSqid := dTask[3].Sqid.String
	aSqid := dTask[0].Sqid.String
	if _, err := s.Move(ctx, dSqid, nil, stack.Position{Before: aSqid}, stack.User); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, d, epic.ID); got != "d,a,b,c" {
		t.Fatalf("after rerank %s", got)
	}
	if _, err := s.Move(ctx, b.Sqid.String, &aSqid, stack.Position{}, stack.User); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, d, epic.ID); got != "d,a,b,c" { // b now nested under a
		t.Fatalf("after reparent %s", got)
	}
	// Cycle: moving a under its own child b is refused.
	bs := b.Sqid.String
	if _, err := s.Move(ctx, aSqid, &bs, stack.Position{}, stack.User); !errors.Is(err, errs.Invalid) {
		t.Fatalf("cycle move: %v", err)
	}
	// Unknown sibling reference.
	if _, err := s.Create(ctx, stack.CreateParams{Project: "p", Parent: epic.Sqid.String, Title: "x", Pos: stack.Position{After: "nope00"}}); !errors.Is(err, errs.Invalid) {
		t.Fatalf("bad sibling: %v", err)
	}
	// Cross-project parent.
	if _, err := s.Create(ctx, stack.CreateParams{Project: "q", Parent: epic.Sqid.String, Title: "x"}); !errors.Is(err, errs.Invalid) {
		t.Fatalf("cross-project parent: %v", err)
	}
	evs, _ := d.RQ.TaskEvents(ctx, b.Sqid.String)
	if len(evs) != 2 || evs[0].Op != "create" || evs[1].Op != "move" || evs[1].Actor != "user" {
		t.Fatalf("events %+v", evs)
	}
}

func TestPrecursorRules(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	epic := mk(t, s, "", "epic", stack.Position{})
	a := mk(t, s, epic.Sqid.String, "a", stack.Position{})
	b := mk(t, s, epic.Sqid.String, "b", stack.Position{})
	c := mk(t, s, "", "c", stack.Position{})
	if err := s.NeedsAdd(ctx, b.Sqid.String, a.Sqid.String, stack.User); err != nil {
		t.Fatal(err)
	}
	if err := s.NeedsAdd(ctx, c.Sqid.String, b.Sqid.String, stack.User); err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]string{
		"cycle":      {a.Sqid.String, c.Sqid.String}, // c->b->a, so a->c cycles
		"self":       {a.Sqid.String, a.Sqid.String},
		"ancestor":   {a.Sqid.String, epic.Sqid.String},
		"descendant": {epic.Sqid.String, a.Sqid.String},
	} {
		if err := s.NeedsAdd(ctx, pair[0], pair[1], stack.User); !errors.Is(err, errs.Invalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := s.NeedsAdd(ctx, b.Sqid.String, a.Sqid.String, stack.User); !errors.Is(err, errs.Conflict) {
		t.Errorf("duplicate: %v", err)
	}
	if err := s.NeedsRm(ctx, b.Sqid.String, a.Sqid.String, stack.User); err != nil {
		t.Fatal(err)
	}
	if err := s.NeedsRm(ctx, b.Sqid.String, a.Sqid.String, stack.User); !errors.Is(err, errs.NotFound) {
		t.Errorf("remove missing: %v", err)
	}
}

func TestContainerStateFollowsChildren(t *testing.T) {
	s, d := setup(t)
	ctx := context.Background()
	epic := mk(t, s, "", "epic", stack.Position{})
	a := mk(t, s, epic.Sqid.String, "a", stack.Position{})
	b := mk(t, s, epic.Sqid.String, "b", stack.Position{})
	if err := s.Complete(ctx, a.Sqid.String, "", stack.User); err != nil {
		t.Fatal(err)
	}
	if got := state(t, s, d, epic.Sqid.String); got != "open" {
		t.Fatalf("one of two done: %s", got)
	}
	if err := s.Complete(ctx, epic.Sqid.String, "", stack.User); !errors.Is(err, errs.Invalid) {
		t.Fatalf("completing a container: %v", err)
	}
	if err := s.Cancel(ctx, b.Sqid.String, "not needed", stack.User); err != nil {
		t.Fatal(err)
	}
	if got := state(t, s, d, epic.Sqid.String); got != "done" {
		t.Fatalf("done + cancelled: %s", got)
	}
	if err := s.Reopen(ctx, a.Sqid.String, stack.User); err != nil {
		t.Fatal(err)
	}
	if got := state(t, s, d, epic.Sqid.String); got != "open" {
		t.Fatalf("after reopen: %s", got)
	}
}

func TestParseOutlineAndSplit(t *testing.T) {
	s, d := setup(t)
	ctx := context.Background()
	nodes, err := stack.ParseOutline("- design\n  - api\n  - schema\n- build\n\n- ship\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 || len(nodes[0].Children) != 2 || nodes[0].Children[1].Title != "schema" {
		t.Fatalf("outline %+v", nodes)
	}
	big := mk(t, s, "", "big job", stack.Position{})
	if _, err := s.Edit(ctx, big.Sqid.String, nil, ptr("shared context"), stack.User); err != nil {
		t.Fatal(err)
	}
	created, err := s.Split(ctx, big.Sqid.String, nodes, true, true, stack.User)
	if err != nil || len(created) != 5 {
		t.Fatalf("split: %d %v", len(created), err)
	}
	if got := titles(t, d, big.ID); got != "design,api,schema,build,ship" {
		t.Fatalf("split order %s", got)
	}
	// chain: build needs design, ship needs build; nested ones are not chained.
	pre, _ := d.RQ.ListPrecursors(ctx, created[3].ID)
	if len(pre) != 1 || pre[0].Title != "design" {
		t.Fatalf("build precursors %+v", pre)
	}
	if created[1].Prompt != "shared context" {
		t.Fatal("copy-prompt not applied")
	}
	if _, err := stack.ParseOutline("\n  \n"); !errors.Is(err, errs.Invalid) {
		t.Fatal("empty outline accepted")
	}
}

func TestQueueChecksAndAgentLoop(t *testing.T) {
	s, d := setup(t)
	ctx := context.Background()
	epic, _ := s.Create(ctx, stack.CreateParams{Project: "p", Title: "epic", Repo: "o/a"})
	a := mk(t, s, epic.Sqid.String, "a", stack.Position{})
	b := mk(t, s, epic.Sqid.String, "b", stack.Position{})
	c := mk(t, s, epic.Sqid.String, "c", stack.Position{})
	other := mk(t, s, "", "elsewhere", stack.Position{})
	if err := s.NeedsAdd(ctx, a.Sqid.String, other.Sqid.String, stack.User); err != nil { // a waits on outside work
		t.Fatal(err)
	}
	if err := s.NeedsAdd(ctx, c.Sqid.String, b.Sqid.String, stack.User); err != nil { // c after b, same run
		t.Fatal(err)
	}

	// Two repos linked and none set: queueing "elsewhere" is ambiguous.
	if _, err := s.Queue(ctx, other.Sqid.String); !errors.Is(err, errs.Invalid) {
		t.Fatalf("ambiguous repo: %v", err)
	}
	run, err := s.Queue(ctx, epic.Sqid.String)
	if err != nil {
		t.Fatal(err)
	}
	if run.Branch != "wrangl/"+epic.Sqid.String+"-1" || !run.TemplateID.Valid || !run.RepoID.Valid {
		t.Fatalf("run %+v", run)
	}
	if _, err := s.Queue(ctx, b.Sqid.String); !errors.Is(err, errs.Conflict) {
		t.Fatalf("overlapping queue: %v", err)
	}
	if err := s.Delete(ctx, b.Sqid.String, stack.User); !errors.Is(err, errs.Conflict) {
		t.Fatalf("delete inside locked subtree: %v", err)
	}

	// Agent loop: a is blocked by outside work, so b first, then c (same-run precursor).
	n, err := s.NextForRun(ctx, run.ID)
	if err != nil || n.Sqid != b.Sqid.String || !strings.Contains(n.Prompt, "Context from the parent tasks") {
		t.Fatalf("first next: %+v %v", n, err)
	}
	if got := state(t, s, d, epic.Sqid.String); got != "in_progress" {
		t.Fatalf("epic while working: %s", got)
	}
	if err := s.AgentComplete(ctx, run.ID, b.Sqid.String, "did b"); err != nil {
		t.Fatal(err)
	}
	n, _ = s.NextForRun(ctx, run.ID)
	if n.Sqid != c.Sqid.String || !strings.Contains(n.Prompt, "did b") {
		t.Fatalf("second next: %+v", n)
	}
	s.AgentComplete(ctx, run.ID, c.Sqid.String, "did c")
	n, _ = s.NextForRun(ctx, run.ID)
	if !n.Blocked || n.Done {
		t.Fatalf("want blocked on outside precursor: %+v", n)
	}
	// Agent cannot complete tasks outside its subtree.
	if err := s.AgentComplete(ctx, run.ID, other.Sqid.String, ""); !errors.Is(err, errs.Forbidden) {
		t.Fatalf("outside subtree: %v", err)
	}
	evs, _ := d.RQ.TaskEvents(ctx, b.Sqid.String)
	if last := evs[len(evs)-1]; last.Actor != "agent:"+itoa(run.ID) || last.Op != "complete" {
		t.Fatalf("agent event %+v", last)
	}
}

func ptr(s string) *string { return &s }

func TestRecordCheckpoint(t *testing.T) {
	s, d := setup(t)
	ctx := context.Background()
	epic := mk(t, s, "", "epic", stack.Position{})
	kid := mk(t, s, epic.Sqid.String, "kid", stack.Position{})
	other := mk(t, s, "", "elsewhere", stack.Position{})
	d.W.Exec(`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('t','','{}',1,1,1)`)
	run, err := d.Q.QueueRun(ctx, dbgen.QueueRunParams{TaskID: epic.ID, Kind: "work", Attempt: 1, Branch: "b", SessionUuid: "u"})
	if err != nil {
		t.Fatal(err)
	}
	sha := "0123456789abcdef0123456789abcdef01234567"
	if err := s.RecordCheckpoint(ctx, run.ID, kid.Sqid.String, sha); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCheckpoint(ctx, run.ID, kid.Sqid.String, "fedcba9876543210fedcba9876543210fedcba98"); err != nil { // again: updates, not duplicates
		t.Fatal(err)
	}
	var n int
	var got string
	d.R.QueryRow(`SELECT COUNT(*), MAX(sha) FROM task_checkpoint`).Scan(&n, &got)
	if n != 1 || got != "fedcba9876543210fedcba9876543210fedcba98" {
		t.Fatalf("rows %d sha %s", n, got)
	}
	if err := s.RecordCheckpoint(ctx, run.ID, kid.Sqid.String, "not a sha; rm -rf"); !errors.Is(err, errs.Invalid) {
		t.Fatalf("bad sha accepted: %v", err)
	}
	if err := s.RecordCheckpoint(ctx, run.ID, other.Sqid.String, sha); !errors.Is(err, errs.Forbidden) {
		t.Fatalf("a run may only checkpoint tasks in its own subtree: %v", err)
	}
}

func TestCreateInArchivedProjectRejected(t *testing.T) {
	s, d := setup(t)
	ctx := context.Background()
	parent := mk(t, s, "", "parent", stack.Position{})
	if _, _, err := d.ArchiveProject(ctx, 1); err != nil {
		t.Fatal(err)
	}
	for _, c := range []stack.CreateParams{
		{Project: "p", Title: "new", Actor: stack.User},
		{Project: "p", Parent: parent.Sqid.String, Title: "child", Actor: stack.Actor("run:1")},
	} {
		_, err := s.Create(ctx, c)
		if !errors.Is(err, errs.Conflict) || !strings.Contains(err.Error(), "archived") {
			t.Fatalf("create %+v: %v", c, err)
		}
	}
	if _, err := d.Q.RestoreProject(ctx, 1); err != nil {
		t.Fatal(err)
	}
	mk(t, s, "", "after restore", stack.Position{})
}

package scheduler

import (
	"context"
	"testing"
	"time"
)

func openItems(t *testing.T, s *Scheduler) (n int, prompt, payload string) {
	t.Helper()
	s.Fleet.DB.R.QueryRow(`SELECT COUNT(*) FROM question WHERE kind = 'unplaceable' AND state = 'open'`).Scan(&n)
	s.Fleet.DB.R.QueryRow(`SELECT COALESCE(MAX(prompt), ''), COALESCE(MAX(payload_json), '') FROM question WHERE kind = 'unplaceable' AND state = 'open'`).Scan(&prompt, &payload)
	return
}

func TestUnplaceableItemLifecycle(t *testing.T) {
	s, _, clk, d := setup(t, 1)
	ctx := context.Background()
	pushes := 0
	s.OnUnplaceable = func(int64, string) { pushes++ }
	if _, err := s.Fleet.CreateTemplate(ctx, []byte("name: alt\narch: arm64\nmin: {vcpu: 2, ram: 4GiB, disk: 20GiB}\nmax_runners: 1\n")); err != nil {
		t.Fatal(err)
	}
	queue(t, d, 1)
	d.W.Exec(`UPDATE run SET template_id = 1`)

	u := Unplaceable{Code: ReasonLaunchBlocked, Prompt: "Can't place abc: launch blocked", Detail: "d", Templates: []string{"alt"}}
	if err := s.NoteUnplaceable(ctx, 1, u); err != nil {
		t.Fatal(err)
	}
	s.NoteUnplaceable(ctx, 1, u) // dedupe
	if n, _, _ := openItems(t, s); n != 1 || pushes != 1 {
		t.Fatalf("open %d pushes %d", n, pushes)
	}
	var opts string
	d.R.QueryRow(`SELECT group_concat(value) FROM question_option ORDER BY position`).Scan(&opts)
	if opts != "wait,use_template:alt,cancel_run" {
		t.Fatalf("options %q", opts)
	}

	// A changed reason rewords the same item.
	s.NoteUnplaceable(ctx, 1, Unplaceable{Code: ReasonNoCapability, Prompt: "Can't place abc: no template", Permanent: true})
	n, prompt, payload := openItems(t, s)
	if n != 1 || prompt != "Can't place abc: no template" || payload != `{"reason":"no_capability"}` {
		t.Fatalf("%d %q %q", n, prompt, payload)
	}
	d.R.QueryRow(`SELECT group_concat(value) FROM question_option`).Scan(&opts)
	if opts != "cancel_run" {
		t.Fatalf("options %q", opts)
	}

	// Answer "wait" on a temporary reason: not raised again for 24h, then raised.
	s.NoteUnplaceable(ctx, 1, u)
	var qid int64
	d.R.QueryRow(`SELECT id FROM question WHERE state = 'open'`).Scan(&qid)
	d.W.Exec(`UPDATE question SET state = 'answered', answered_at = ?, answer_option_id = (SELECT id FROM question_option WHERE question_id = ? AND value = 'wait') WHERE id = ?`,
		clk.Now().UTC().Format("2006-01-02T15:04:05.000Z"), qid, qid)
	s.NoteUnplaceable(ctx, 1, u)
	if n, _, _ := openItems(t, s); n != 0 {
		t.Fatal("raised again right after keep waiting")
	}
	clk.Advance(25 * time.Hour)
	s.NoteUnplaceable(ctx, 1, u)
	if n, _, _ := openItems(t, s); n != 1 {
		t.Fatal("not raised again after 24h")
	}

	// Assign resolves items whose run is no longer queued.
	d.W.Exec(`UPDATE run SET state = 'cancelled'`)
	s.Assign(ctx)
	var state, text string
	d.R.QueryRow(`SELECT state, answer_text FROM question WHERE state <> 'answered'`).Scan(&state, &text)
	if state != "cancelled" || text != "resolved: placeable" {
		t.Fatalf("%q %q", state, text)
	}
}

func TestUnplaceableAnswers(t *testing.T) {
	s, _, _, d := setup(t, 1)
	ctx := context.Background()
	var cancelled int64
	s.CancelRun = func(_ context.Context, id int64) error { cancelled = id; return nil }
	if _, err := s.Fleet.CreateTemplate(ctx, []byte("name: alt\narch: arm64\nmin: {vcpu: 2, ram: 4GiB, disk: 20GiB}\nmax_runners: 1\n")); err != nil {
		t.Fatal(err)
	}
	queue(t, d, 1)
	d.W.Exec(`UPDATE run SET template_id = 1`)
	s.NoteUnplaceable(ctx, 1, Unplaceable{Code: ReasonCapacity, Prompt: "p", Templates: []string{"alt"}})
	q, err := d.RQ.GetQuestion(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Answer(ctx, q, "use_template:alt", ""); err != nil {
		t.Fatal(err)
	}
	var task, run int64
	d.R.QueryRow(`SELECT template_id FROM task`).Scan(&task)
	d.R.QueryRow(`SELECT template_id FROM run`).Scan(&run)
	if task != 2 || run != 2 {
		t.Fatalf("template not switched: task %d run %d", task, run)
	}
	if err := s.Answer(ctx, q, "cancel_run", ""); err != nil || cancelled != 1 {
		t.Fatalf("cancel: %v %d", err, cancelled)
	}
}

func TestUnplaceableRaisedForStageOneReasons(t *testing.T) {
	s, _, clk, d := setup(t, 1)
	ctx := context.Background()
	d.W.Exec(`UPDATE runner_template SET agent_json = '{"start":["true"]}'`)
	d.W.Exec(`INSERT INTO repo (project_id, owner, name, default_branch, clone_url) VALUES (1, 'o', 'n', 'main', 'file:///x')`)

	// A run whose template is gone: at once.
	queue(t, d, 1)
	d.W.Exec(`UPDATE run SET repo_id = 1`)
	s.Assign(ctx)
	if n, _, p := openItems(t, s); n != 1 || p != `{"reason":"no_capability"}` {
		t.Fatalf("no_capability: %d %s", n, p)
	}

	// A failed template version: only after 15 minutes of waiting.
	d.W.Exec(`UPDATE run SET state = 'cancelled'`)
	s.Assign(ctx) // resolves the first item
	queue(t, d, 1)
	d.W.Exec(`UPDATE run SET template_id = 1, repo_id = 1 WHERE state = 'queued'`)
	d.W.Exec(`UPDATE runner_template SET failed_version = version`)
	s.Assign(ctx)
	if n, _, _ := openItems(t, s); n != 0 {
		t.Fatal("raised before the wait elapsed")
	}
	clk.Advance(10 * time.Minute)
	s.Assign(ctx)
	if n, _, _ := openItems(t, s); n != 0 {
		t.Fatal("raised after 10 minutes")
	}
	clk.Advance(6 * time.Minute)
	s.Assign(ctx)
	if n, _, p := openItems(t, s); n != 1 || p != `{"reason":"launch_blocked"}` {
		t.Fatalf("launch_blocked: %d %s", n, p)
	}

	// A usable runner means the run just waits for it: the item resolves when the cause is gone.
	d.W.Exec(`UPDATE runner_template SET failed_version = 0`)
	d.W.Exec(`UPDATE project_template_limit SET max_instances = 0`)
	s.Assign(ctx)
	if n, _, p := openItems(t, s); n != 1 || p != `{"reason":"capacity"}` {
		t.Fatalf("capacity: %d %s", n, p)
	}
	d.W.Exec(`INSERT INTO runner (name, template_id, state, template_version) VALUES ('r1', 1, 'launching', 1)`)
	s.Assign(ctx)
	if n, _, p := openItems(t, s); n != 1 || p != `{"reason":"capacity"}` {
		t.Fatalf("item should stay until the run is placed: %d %s", n, p)
	}
}

package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
)

func TestWaitReasonGolden(t *testing.T) {
	for _, c := range []struct {
		full, starting, unavail int64
		want                    string
	}{
		{0, 0, 0, "waiting: no runner of template t yet"},
		{2, 0, 0, "waiting: no free slot on template t (2 full)"},
		{1, 1, 0, "waiting: no free slot on template t (1 full, 1 starting)"},
		{0, 0, 3, "waiting: no free slot on template t (3 cordoned, draining or updating)"},
		{1, 2, 3, "waiting: no free slot on template t (1 full, 2 starting, 3 cordoned, draining or updating)"},
	} {
		if got := WaitReason("t", c.full, c.starting, c.unavail); got != c.want {
			t.Errorf("WaitReason(%d,%d,%d) = %q, want %q", c.full, c.starting, c.unavail, got, c.want)
		}
	}
	if got := WaitReasonTarget("mac"); got != "waiting: no free target for template mac" {
		t.Errorf("target reason: %q", got)
	}
}

func TestQueuedRunSaysWhyItWaitsOnlyOnChange(t *testing.T) {
	s, _, clk, d := setup(t, 1)
	ctx := context.Background()
	d.W.Exec(`UPDATE runner_template SET agent_json = '{"start":["true"]}', max_sessions = 1`)
	d.W.Exec(`INSERT INTO repo (project_id, owner, name, default_branch, clone_url) VALUES (1, 'o', 'n', 'main', 'file:///x')`)
	queue(t, d, 1)
	d.W.Exec(`UPDATE run SET template_id = 1, repo_id = 1`)
	get := func() (reason, since string) {
		d.R.QueryRow(`SELECT blocked_reason, COALESCE(blocked_since, '') FROM run`).Scan(&reason, &since)
		return
	}

	if _, err := s.Assign(ctx); err != nil {
		t.Fatal(err)
	}
	if r, _ := get(); r != "waiting: no runner of template "+tmplName(d)+" yet" {
		t.Fatalf("no runner: %q", r)
	}
	var status string
	d.R.QueryRow(`SELECT status_text FROM run`).Scan(&status)
	r1, since1 := get()
	if status != r1 || since1 == "" {
		t.Fatalf("status %q reason %q since %q", status, r1, since1)
	}

	// Same reason later: nothing is rewritten, blocked_since stays.
	clk.Advance(time.Hour)
	s.Assign(ctx)
	if _, since2 := get(); since2 != since1 {
		t.Fatalf("blocked_since moved: %q -> %q", since1, since2)
	}

	// A runner that is still starting changes the text; the run is still blocked since the same time.
	d.W.Exec(`INSERT INTO runner (name, template_id, state, template_version) VALUES ('r1', 1, 'launching', 1)`)
	s.Assign(ctx)
	if r, since := get(); r != "waiting: no free slot on template "+tmplName(d)+" (1 starting)" || since != since1 {
		t.Fatalf("starting: %q since %q", r, since)
	}

	// A full runner.
	d.W.Exec(`UPDATE runner SET state = 'busy' WHERE name = 'r1'`)
	d.W.Exec(`INSERT INTO task (project_id, rank, title) VALUES (1, 'V999', 'other')`)
	d.W.Exec(`INSERT INTO run (task_id, state, runner_id, template_id, repo_id) VALUES ((SELECT id FROM task WHERE rank = 'V999'), 'running', (SELECT id FROM runner), 1, 1)`)
	s.Assign(ctx)
	if r, _ := get(); r != "waiting: no free slot on template "+tmplName(d)+" (1 full)" {
		t.Fatalf("full: %q", r)
	}

	// Once a slot frees, the run is claimed and the reason is cleared on assignment (even if the fake runner then declines).
	d.W.Exec(`UPDATE run SET state = 'succeeded' WHERE state = 'running'`)
	s.Assign(ctx)
	var state, reason string
	d.R.QueryRow(`SELECT state, blocked_reason FROM run WHERE task_id = (SELECT id FROM task WHERE rank <> 'V999')`).Scan(&state, &reason)
	if state != "queued" && reason != "" {
		t.Fatalf("assigned run still blocked: %q", reason)
	}
}

func tmplName(d *db.DB) string {
	var n string
	d.R.QueryRow(`SELECT name FROM runner_template WHERE id = 1`).Scan(&n)
	return n
}

func TestAllowedHostIsUsedFirstAndCountsAsSupply(t *testing.T) {
	s, fake, _, d := setup(t, 2)
	ctx := context.Background()
	if _, err := s.Fleet.CreateTemplate(ctx, []byte("name: host\narch: arm64\nmin: {vcpu: 2, ram: 4GiB, disk: 20GiB}\nmax_runners: 1\n")); err != nil {
		t.Fatal(err)
	}
	d.W.Exec(`UPDATE runner_template SET agent_json = '{"start":["true"]}'`)
	d.W.Exec(`INSERT INTO runner (name, template_id, state, idle_since, template_version) VALUES ('h1', 2, 'idle', '2026-01-01T00:00:00.000Z', (SELECT version FROM runner_template WHERE id = 2))`)
	d.W.Exec(`INSERT INTO repo (project_id, owner, name, default_branch, clone_url) VALUES (1, 'o', 'n', 'main', 'file:///x')`)
	queue(t, d, 1)
	d.W.Exec(`UPDATE run SET template_id = 1, repo_id = 1`)

	// Not allowed: the host is invisible, the run waits and the autoscaler launches.
	if _, err := s.Assign(ctx); err != nil {
		t.Fatal(err)
	}
	var reason string
	d.R.QueryRow(`SELECT blocked_reason FROM run`).Scan(&reason)
	if reason == "" {
		t.Fatal("the run should wait when the host is not allowed")
	}
	if got, _ := s.Tick(ctx); got["go"] != 1 {
		t.Fatalf("expected a launch without the allowance: %v", got)
	}

	// Allowed: the host's free slots are supply (no launch) and the run is tried on the host first.
	fake.Runs = nil
	d.W.Exec(`UPDATE runner SET state = 'terminated' WHERE template_id = 1`)
	d.W.Exec(`INSERT INTO project_allowed_template (project_id, template_id) VALUES (1, 2)`)
	if got, _ := s.Tick(ctx); len(got) != 0 {
		t.Fatalf("launched although the allowed host has room: %v", got)
	}
	_, err := s.Assign(ctx)
	if err == nil || !strings.Contains(err.Error(), "h1") {
		t.Fatalf("the run should have been offered to the host runner h1, got %v", err)
	}

	// Host full: overflow launches again.
	d.W.Exec(`UPDATE runner_template SET max_sessions = 1 WHERE id = 2`)
	d.W.Exec(`UPDATE runner SET state = 'busy' WHERE name = 'h1'`)
	d.W.Exec(`INSERT INTO task (project_id, rank, title) VALUES (1, 'V998', 'x')`)
	d.W.Exec(`INSERT INTO run (task_id, state, runner_id) VALUES ((SELECT id FROM task WHERE rank = 'V998'), 'running', (SELECT id FROM runner WHERE name = 'h1'))`)
	if got, _ := s.Tick(ctx); got["go"] != 1 {
		t.Fatalf("expected overflow launch when the host is full: %v", got)
	}
}

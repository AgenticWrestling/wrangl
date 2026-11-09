package scheduler

import (
	"context"
	"testing"
)

// A run's requirements steer the claim to a runner that has the tag, and an agent: requirement keeps a run off
// runners of a template that does not allow that agent.
func TestRequirementsSteerTheClaim(t *testing.T) {
	ctx := context.Background()
	s, _, _, d := setup(t, 3)
	loadFixtures(t, d, []fixture{
		{state: "idle", version: 1, idle: "2026-01-01T00:00:00.000Z", launched: 1}, // longest idle: would win without requirements
		{state: "idle", version: 1, idle: "2026-01-02T00:00:00.000Z", launched: 1},
	})
	d.W.Exec(`INSERT INTO runner_capability (runner_id, key, value, source) VALUES (1, 'arch', 'arm64', 'template'), (2, 'arch', 'arm64', 'template'), (2, 'gpu', '', 'manual')`)
	queue(t, d, 1)
	d.W.Exec(`UPDATE run SET template_id = 1`)
	got, err := s.claimRunner(ctx, 1, 1)
	if err != nil || got.ID != 1 {
		t.Fatalf("without requirements the longest idle wins: %v %v", got, err)
	}
	d.W.Exec(`UPDATE runner SET state = 'idle' WHERE id = 1`)
	d.W.Exec(`INSERT INTO requirement (owner_kind, owner_id, key, op, value) VALUES ('run', 1, 'gpu', 'exists', '')`)
	got, err = s.claimRunner(ctx, 1, 1)
	if err != nil || got.ID != 2 {
		t.Fatalf("the requirement picks the runner that has the tag: %v %v", got, err)
	}
	d.W.Exec(`UPDATE requirement SET key = 'agent:codex'`)
	if _, err := s.claimRunner(ctx, 1, 1); err == nil {
		t.Fatal("no runner has agent:codex")
	}
}

// A requirement no template provides raises a permanent "can't be placed" item at once; changing the
// requirement resolves it.
func TestNoCapabilityItemAndResolve(t *testing.T) {
	ctx := context.Background()
	s, fake, _, d := setup(t, 3)
	d.W.Exec(`UPDATE runner_template SET agent_json = '{"start":["true"]}'`)
	queue(t, d, 1)
	d.W.Exec(`UPDATE run SET template_id = 1`)
	d.W.Exec(`INSERT INTO requirement (owner_kind, owner_id, key, op, value) VALUES ('run', 1, 'gpu', 'exists', '')`)
	s.Tick(ctx)
	var n int
	var prompt string
	d.R.QueryRow(`SELECT COUNT(*), COALESCE(MAX(prompt), '') FROM question WHERE kind = 'unplaceable' AND state = 'open' AND json_extract(payload_json, '$.reason') = 'no_capability'`).Scan(&n, &prompt)
	if n != 1 || len(fake.Runs) != 0 || prompt == "" {
		t.Fatalf("items %d prompt %q launches %d", n, prompt, len(fake.Runs))
	}
	d.W.Exec(`DELETE FROM requirement`)
	s.Tick(ctx)
	d.R.QueryRow(`SELECT COUNT(*) FROM question WHERE kind = 'unplaceable' AND state = 'open'`).Scan(&n)
	if n != 0 || len(fake.Runs) != 1 {
		t.Fatalf("open items %d, launches %d", n, len(fake.Runs))
	}
}

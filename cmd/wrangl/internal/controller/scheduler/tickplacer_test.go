package scheduler

import (
	"context"
	"database/sql"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/placer"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
)

// Replay: with the default 0/0/0 needs, the placer engine launches exactly what the legacy Tick launches.
func TestPlacerEngineMatchesLegacyTickWithDefaultNeeds(t *testing.T) {
	ctx := context.Background()
	sets := [][]fixture{
		nil,
		{fixtures[0]},
		{fixtures[2]},              // one busy runner with room
		{fixtures[4]},              // a runner at the session ceiling
		{fixtures[5], fixtures[6]}, // cordoned and draining: not supply
		{fixtures[7]},              // behind on the template version
		{fixtures[9], fixtures[3]}, // one launching, one nearly full
		fixtures[:4],
		fixtures,
	}
	for _, maxRunners := range []int{2, 9} {
		for _, queued := range []int{0, 1, 5, 9} {
			for n, fx := range sets {
				run := func(engine string) (map[string]int, int) {
					s, fake, _, d := setup(t, maxRunners)
					s.Engine = engine
					loadFixtures(t, d, fx)
					queue(t, d, queued)
					d.W.Exec(`UPDATE run SET template_id = 1`)
					got, _ := s.Tick(ctx)
					return got, len(fake.Runs)
				}
				lg, lr := run("")
				pg, pr := run(EnginePlacer)
				if lg["go"] != pg["go"] || lr != pr {
					t.Fatalf("max_runners %d, %d queued, fixture set %d: legacy launched %v (%d instances), placer %v (%d)", maxRunners, queued, n, lg, lr, pg, pr)
				}
			}
		}
	}
}

// With real needs the placer packs by resources, not slots: three 3 GiB runs do not share a 4 GiB box.
func TestPlacerEngineLaunchesByNeed(t *testing.T) {
	ctx := context.Background()
	s, fake, _, d := setup(t, 6)
	s.Engine = EnginePlacer
	queue(t, d, 3)
	d.W.Exec(`UPDATE run SET template_id = 1, need_vcpu = 1, need_ram_mib = 3072, need_disk_gib = 5`)
	// An enrolled 4 vCPU / 4 GiB runner: 3 GiB usable after the reserve, so one run fits, not four slots' worth.
	d.W.Exec(`UPDATE runner_template SET max_sessions = 4`)
	d.W.Exec(`INSERT INTO runner (name, template_id, state, idle_since, template_version, launched, vcpu, ram_mib, disk_gib) VALUES ('e', 1, 'idle', '2026-01-01T00:00:00.000Z', 1, 1, 4, 4096, 30)`)
	got, err := s.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// One run fits the existing runner (3072 <= 3072); the other two need launches. The legacy template
	// has no menu, so each launch is a single legacy entry with unbounded capacity hosting both.
	if got["go"] != 1 || len(fake.Runs) != 1 {
		t.Fatalf("launched %v (%d instances)", got, len(fake.Runs))
	}
}

func TestTooBigRaisesAPermanentItemAndResolvesWhenNeedsShrink(t *testing.T) {
	ctx := context.Background()
	s, fake, _, d := setup(t, 6)
	s.Engine = EnginePlacer
	if _, err := s.Fleet.CreateTemplate(ctx, []byte("name: alt\narch: arm64\nmin: {vcpu: 2, ram: 4GiB, disk: 20GiB}\nmax_runners: 1\n")); err != nil {
		t.Fatal(err)
	}
	queue(t, d, 1)
	// The legacy template has unbounded capacity, so use a runner-less template with a bounded menu entry
	// by pre-seeding the menu cache.
	d.W.Exec(`UPDATE run SET template_id = 1, need_vcpu = 1, need_ram_mib = 65536, need_disk_gib = 5`)
	tt, _ := d.RQ.GetTemplate(ctx, 1)
	s.menus = map[int64]cachedMenu{1: {version: tt.Version, at: s.Now(), entries: []placer.Entry{{Template: 1, Type: "m7g.2xlarge", Cap: placer.Need{VCPU: 8, RAMMiB: 31744, DiskGiB: 400}, Ceiling: 4}}}}
	s.Tick(ctx)
	var n int
	var prompt, opts string
	d.R.QueryRow(`SELECT COUNT(*), COALESCE(MAX(prompt), '') FROM question WHERE kind = 'unplaceable' AND state = 'open' AND json_extract(payload_json, '$.reason') = 'too_big'`).Scan(&n, &prompt)
	d.R.QueryRow(`SELECT group_concat(value) FROM (SELECT value FROM question_option ORDER BY position)`).Scan(&opts)
	if n != 1 || len(fake.Runs) != 0 || prompt == "" || opts != "edit_needs,use_template:alt,cancel_run" {
		t.Fatalf("items %d prompt %q options %q launches %d", n, prompt, opts, len(fake.Runs))
	}
	// Lowering the needs from the item re-snapshots the run; the next tick resolves the item and launches.
	// (stack.SetNeed does this; here the run is updated directly.)
	d.W.Exec(`UPDATE run SET need_ram_mib = 4096`)
	s.Tick(ctx)
	d.R.QueryRow(`SELECT COUNT(*) FROM question WHERE kind = 'unplaceable' AND state = 'open'`).Scan(&n)
	if n != 0 || len(fake.Runs) != 1 {
		t.Fatalf("open items %d, launches %d", n, len(fake.Runs))
	}
}

func TestPlacerEngineStampsPriceAndNotesApproachingBudget(t *testing.T) {
	ctx := context.Background()
	s, fake, _, d := setup(t, 6)
	s.Engine = EnginePlacer
	queue(t, d, 1)
	d.W.Exec(`UPDATE run SET template_id = 1`)
	tt, _ := d.RQ.GetTemplate(ctx, 1)
	price := 0.40
	s.menus = map[int64]cachedMenu{1: {version: tt.Version, at: s.Now(), entries: []placer.Entry{{Template: 1, Type: "m7g.large", Cap: placer.Need{VCPU: 2, RAMMiB: 7168, DiskGiB: 400}, Ceiling: 4, HourlyUSD: &price}}}}
	d.W.Exec(`UPDATE placement_config SET budget_hourly_usd = 0.50`)
	if got, _ := s.Tick(ctx); got["go"] != 1 || len(fake.Runs) != 1 {
		t.Fatalf("launched %v", got)
	}
	var usd float64
	d.R.QueryRow(`SELECT hourly_usd FROM runner_capacity`).Scan(&usd)
	if usd < 0.40 || usd > 0.45 {
		t.Fatalf("the launch price (instance plus volume) should be stamped: %f", usd)
	}
	var n int
	d.R.QueryRow(`SELECT COUNT(*) FROM log_entry WHERE title LIKE 'Hourly spend%'`).Scan(&n)
	if n != 1 {
		t.Fatalf("one approaching note expected, got %d", n)
	}
	s.Tick(ctx)
	d.R.QueryRow(`SELECT COUNT(*) FROM log_entry WHERE title LIKE 'Hourly spend%'`).Scan(&n)
	if n != 1 {
		t.Fatalf("the note is once per crossing, got %d", n)
	}
}

func TestBudgetGateReasonAndReclaim(t *testing.T) {
	ctx := context.Background()
	s, fake, _, d := setup(t, 6)
	s.Engine = EnginePlacer
	// An idle launched runner that costs 0.47/h and is too small for the queued run (so it cannot host it).
	old, err := s.Fleet.Launch(ctx, "go")
	if err != nil {
		t.Fatal(err)
	}
	d.W.Exec(`UPDATE runner SET state = 'idle', idle_since = '2026-01-01T00:00:00.000Z', template_version = 1, vcpu = 2, ram_mib = 4096, disk_gib = 30 WHERE id = ?`, old.ID)
	d.Q.SetRunnerPrice(ctx, dbgenPrice(old.ID, 0.47))
	queue(t, d, 1)
	d.W.Exec(`UPDATE run SET template_id = 1, need_vcpu = 4, need_ram_mib = 6000, need_disk_gib = 5`)
	tt, _ := d.RQ.GetTemplate(ctx, 1)
	price := 0.145
	s.menus = map[int64]cachedMenu{1: {version: tt.Version, at: s.Now(), entries: []placer.Entry{{Template: 1, Type: "m7g.large", Cap: placer.Need{VCPU: 4, RAMMiB: 7168, DiskGiB: 400}, Ceiling: 4, HourlyUSD: &price}}}}
	d.W.Exec(`UPDATE placement_config SET budget_hourly_usd = 0.50`)
	before := len(fake.Runs)
	s.Tick(ctx)
	var reason, state string
	d.R.QueryRow(`SELECT blocked_reason FROM run`).Scan(&reason)
	d.R.QueryRow(`SELECT state FROM runner WHERE id = ?`, old.ID).Scan(&state)
	if reason != "waiting: hourly budget $0.50 reached ($0.47 in use; m7g.large would add $0.145)" {
		t.Fatalf("reason %q", reason)
	}
	if state != "terminated" {
		t.Fatalf("the idle runner that cannot host the run should be reclaimed, state %s", state)
	}
	if len(fake.Runs) != before {
		t.Fatal("nothing may launch past the ceiling in the same pass")
	}
	// With the idle runner gone the next tick has room and launches; the stale reason is cleared.
	got, terr := s.Tick(ctx)
	t.Log(got, terr)
	d.R.QueryRow(`SELECT blocked_reason FROM run`).Scan(&reason)
	if len(fake.Runs) != before+1 || reason != "" {
		t.Fatalf("launches %d, reason %q", len(fake.Runs)-before, reason)
	}
}

func dbgenPrice(id int64, usd float64) dbgen.SetRunnerPriceParams {
	return dbgen.SetRunnerPriceParams{RunnerID: id, HourlyUsd: sql.NullFloat64{Float64: usd, Valid: true}}
}

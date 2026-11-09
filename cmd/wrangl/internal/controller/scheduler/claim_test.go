package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/placer"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/placement"
)

type fixture struct {
	state           string
	cordoned, drain int
	version         int // template_version; the template is at 1
	upgrade         string
	idle            string
	launched        int
	active          int     // runs in state running on it
	need            []int64 // need of each active run: vcpu, ram, disk (repeated per run)
}

var fixtures = []fixture{
	{state: "idle", version: 1, idle: "2026-01-02T00:00:00.000Z", launched: 1},
	{state: "idle", version: 1, idle: "2026-01-01T00:00:00.000Z", launched: 1},
	{state: "busy", version: 1, launched: 1, active: 1},
	{state: "busy", version: 1, launched: 1, active: 3},
	{state: "busy", version: 1, launched: 1, active: 4},   // at the ceiling
	{state: "idle", version: 1, cordoned: 1, launched: 1}, // cordoned
	{state: "idle", version: 1, drain: 1, launched: 1},    // draining
	{state: "idle", version: 0, launched: 1},              // behind on the template version
	{state: "idle", version: 1, upgrade: "abc", launched: 1},
	{state: "launching", version: 1, launched: 1},
	{state: "offline", version: 1, launched: 1},
	{state: "terminated", version: 1, launched: 1},
	{state: "busy", version: 1, launched: 0, active: 2}, // the adopted host
}

func loadFixtures(t *testing.T, d *db.DB, fx []fixture) {
	t.Helper()
	d.W.Exec(`UPDATE runner_template SET max_sessions = 4`)
	for i, f := range fx {
		idle := any(nil)
		if f.idle != "" {
			idle = f.idle
		}
		if _, err := d.W.Exec(`INSERT INTO runner (name, template_id, state, idle_since, template_version, cordoned, drain_requested, upgrade_sha, launched, vcpu, ram_mib, disk_gib)
			VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?, 4, 8192, 30)`, fmt.Sprintf("r%d", i), f.state, idle, f.version, f.cordoned, f.drain, f.upgrade, f.launched); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < f.active; j++ {
			need := f.need
			if len(need) != 3 {
				need = []int64{0, 0, 0}
			}
			d.W.Exec(`INSERT INTO task (project_id, rank, title) VALUES (1, ?, 't')`, fmt.Sprintf("F%d-%d", i, j))
			d.W.Exec(`INSERT INTO run (task_id, state, runner_id, need_vcpu, need_ram_mib, need_disk_gib) VALUES ((SELECT MAX(id) FROM task), 'running', ?, ?, ?, ?)`, i+1, need[0], need[1], need[2])
		}
	}
}

// With 0/0/0 needs and max_sessions 4, the new claim picks exactly the runner the old ClaimIdleRunner picked.
func TestClaimRunnerMatchesClaimIdleRunnerWithDefaultNeeds(t *testing.T) {
	ctx := context.Background()
	// Every single fixture alone, every pair, and all together.
	var sets [][]fixture
	for i := range fixtures {
		sets = append(sets, []fixture{fixtures[i]})
		for j := i + 1; j < len(fixtures); j++ {
			sets = append(sets, []fixture{fixtures[i], fixtures[j]})
		}
	}
	sets = append(sets, fixtures)
	for n, fx := range sets {
		oldS, _, _, oldD := setup(t, 4)
		loadFixtures(t, oldD, fx)
		queue(t, oldD, 1)
		want, werr := oldD.Q.ClaimIdleRunner(ctx, 1)

		newS, _, _, newD := setup(t, 4)
		loadFixtures(t, newD, fx)
		queue(t, newD, 1)
		got, gerr := newS.claimRunner(ctx, 1, 1)
		_ = oldS
		if (werr == nil) != (gerr == nil) || (werr != nil && !(werr == sql.ErrNoRows && gerr == sql.ErrNoRows)) {
			t.Fatalf("set %d: old err %v, new err %v", n, werr, gerr)
		}
		if werr == nil && want.ID != got.ID {
			t.Fatalf("set %d: old claimed runner %d, new %d", n, want.ID, got.ID)
		}
	}
}

// ClaimRunner (SQL) and placer.Fits (Go) agree on which runners a need fits, with real needs.
func TestClaimRunnerAgreesWithFits(t *testing.T) {
	ctx := context.Background()
	fx := []fixture{
		{state: "idle", version: 1, launched: 1},                                       // 4 vCPU, 7 GiB, 22 GiB
		{state: "busy", version: 1, launched: 1, active: 2, need: []int64{1, 2048, 5}}, // half used
		{state: "busy", version: 1, launched: 1, active: 1, need: []int64{4, 100, 1}},  // vcpu used up
		{state: "busy", version: 1, launched: 0, active: 1, need: []int64{1, 2048, 5}}, // the host: 3 vCPU / 6 GiB / 14 GiB
		{state: "idle", version: 1, cordoned: 1, launched: 1},
	}
	needs := []placer.Need{{}, {1, 1024, 2}, {1, 2048, 5}, {2, 4096, 10}, {4, 7168, 22}, {5, 1, 1}, {1, 8000, 1}, {1, 1, 30}, {3, 6144, 14}}
	for _, need := range needs {
		s, _, _, d := setup(t, 4)
		loadFixtures(t, d, fx)
		rows, _ := d.RQ.ListRunners(ctx, 0)
		for _, row := range rows {
			r := row.Runner
			c, err := (&placement.Service{DB: d}).RunnerCapacity(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			active, _ := d.RQ.CountActiveRunsForRunner(ctx, sql.NullInt64{Int64: r.ID, Valid: true})
			ceil := int64(4)
			claimable := (r.State == "idle" || r.State == "busy") && r.Cordoned == 0 && r.DrainRequested == 0 && r.UpgradeSha == ""
			pr := placer.Runner{ID: r.ID, Claimable: claimable, Cap: placer.Need{VCPU: c.VCPU, RAMMiB: c.RAMMiB, DiskGiB: c.DiskGiB},
				Used: placer.Need{VCPU: c.UsedVCPU, RAMMiB: c.UsedRAMMiB, DiskGiB: c.UsedDiskGiB}, Active: active, Ceiling: &ceil}
			_, err = d.Q.ClaimRunner(ctx, dbgen.ClaimRunnerParams{ID: r.ID, NeedVcpu: need.VCPU, NeedRamMib: need.RAMMiB, NeedDiskGib: need.DiskGiB})
			if (err == nil) != placer.Fits(need, pr) {
				t.Errorf("need %+v runner %s: ClaimRunner ok=%v, Fits=%v (cap %+v used %+v)", need, r.Name, err == nil, placer.Fits(need, pr), pr.Cap, pr.Used)
			}
		}
		_ = s
	}
}

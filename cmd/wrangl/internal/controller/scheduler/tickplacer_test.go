package scheduler

import (
	"context"
	"testing"
)

// Replay: with the default 0/0/0 needs, the placer engine launches exactly what the legacy Tick launches.
func TestPlacerEngineMatchesLegacyTickWithDefaultNeeds(t *testing.T) {
	ctx := context.Background()
	sets := [][]fixture{
		nil,
		{fixtures[0]},
		{fixtures[2]},                // one busy runner with room
		{fixtures[4]},                // a runner at the session ceiling
		{fixtures[5], fixtures[6]},   // cordoned and draining: not supply
		{fixtures[7]},                // behind on the template version
		{fixtures[9], fixtures[3]},   // one launching, one nearly full
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

package placement

import (
	"context"
	"database/sql"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
)

func TestDeriveCapacity(t *testing.T) {
	// A launched 4 vCPU / 8 GiB / 30 GiB box: facts minus the reserve.
	if v, r, d := DeriveCapacity(true, 4, 8192, 30); v != 4 || r != 7168 || d != 22 {
		t.Fatalf("launched: %d %d %d", v, r, d)
	}
	// The host: declared 6 / 12 GiB / 20 GiB, capped to 75% / 75% / 70% of a smaller box.
	if v, r, d := DeriveCapacity(false, 8, 31*1024, 100); v != 6 || r != 12*1024 || d != 20 {
		t.Fatalf("host: %d %d %d", v, r, d)
	}
	if v, r, d := DeriveCapacity(false, 4, 8192, 20); v != 3 || r != 6144 || d != 14 {
		t.Fatalf("small host: %d %d %d", v, r, d)
	}
	if _, r, _ := DeriveCapacity(true, 2, 512, 4); r != 0 {
		t.Fatalf("never negative: %d", r)
	}
}

func TestRunnerCapacityExplicitAndUsed(t *testing.T) {
	s, d := setup(t)
	ctx := context.Background()
	d.W.Exec(`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('host','','{}',1,1,1)`)
	d.W.Exec(`INSERT INTO runner (name, template_id, state, launched, vcpu, ram_mib, disk_gib) VALUES ('h', 1, 'idle', 0, 8, 31744, 100)`)
	d.W.Exec(`INSERT INTO run (task_id, state, runner_id, need_vcpu, need_ram_mib, need_disk_gib) VALUES (1, 'running', 1, 1, 2048, 5)`)
	d.W.Exec(`INSERT INTO run (task_id, state, runner_id, need_vcpu, need_ram_mib, need_disk_gib) VALUES (1, 'succeeded', 1, 1, 2048, 5)`)
	r, _ := d.RQ.GetRunnerByName(ctx, "h")
	c, err := s.RunnerCapacity(ctx, r)
	if err != nil || c.VCPU != 6 || c.RAMMiB != 12288 || c.DiskGiB != 20 || c.Explicit || c.UsedVCPU != 1 || c.UsedRAMMiB != 2048 || c.UsedDiskGiB != 5 {
		t.Fatalf("%+v %v", c, err)
	}
	d.Q.InitRunnerCapacity(ctx, dbInit(1, 4))
	if err := s.SetRunnerCapacity(ctx, 1, map[string]string{"vcpu": "7", "ram": "16GiB"}); err != nil {
		t.Fatal(err)
	}
	c, _ = s.RunnerCapacity(ctx, r)
	if c.VCPU != 7 || c.RAMMiB != 16384 || c.DiskGiB != 20 || !c.Explicit || c.MaxSessions == nil || *c.MaxSessions != 4 {
		t.Fatalf("%+v", c)
	}
	if err := s.SetRunnerCapacity(ctx, 1, map[string]string{"vcpu": ""}); err != nil {
		t.Fatal(err)
	}
	if c, _ = s.RunnerCapacity(ctx, r); c.VCPU != 6 {
		t.Fatalf("cleared field should derive again: %+v", c)
	}
	if err := s.SetRunnerCapacity(ctx, 1, map[string]string{"ram": "100MiB"}); err == nil {
		t.Fatal("floor not enforced")
	}
}

func dbInit(id, max int64) dbgen.InitRunnerCapacityParams {
	return dbgen.InitRunnerCapacityParams{RunnerID: id, MaxSessions: sql.NullInt64{Int64: max, Valid: true}}
}

func TestHostCapacityLeavesRoomForPrimarySessions(t *testing.T) {
	s, d := setup(t)
	ctx := context.Background()
	hb := `{"claude":[{"pid":1,"kind":"primary"},{"pid":2,"kind":"primary"},{"pid":3,"kind":"run"},{"pid":4,"kind":"other"}]}`
	d.W.Exec(`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('host','','{}',1,1,1)`)
	d.W.Exec(`INSERT INTO runner (name, template_id, state, launched, vcpu, ram_mib, disk_gib, last_metrics_json) VALUES ('h', 1, 'idle', 0, 8, 31744, 100, ?)`, hb)
	d.W.Exec(`INSERT INTO runner (name, template_id, state, launched, vcpu, ram_mib, disk_gib, last_metrics_json) VALUES ('l', 1, 'idle', 1, 8, 31744, 100, ?)`, hb)
	h, _ := d.RQ.GetRunnerByName(ctx, "h")
	c, _ := s.RunnerCapacity(ctx, h)
	if want := int64(12*1024 - 2*HostPrimaryReserveMiB); c.RAMMiB != want {
		t.Errorf("host ram %d, want %d (two primaries reserved)", c.RAMMiB, want)
	}
	l, _ := d.RQ.GetRunnerByName(ctx, "l")
	if c, _ := s.RunnerCapacity(ctx, l); c.RAMMiB != 31744-ReserveRAMMiB {
		t.Errorf("a launched runner's reserve changed: %d", c.RAMMiB)
	}
	// An owner's explicit RAM is taken as given.
	d.Q.InitRunnerCapacity(ctx, dbInit(h.ID, 4))
	s.SetRunnerCapacity(ctx, h.ID, map[string]string{"ram": "12GiB"})
	if c, _ := s.RunnerCapacity(ctx, h); c.RAMMiB != 12288 {
		t.Errorf("explicit ram altered: %d", c.RAMMiB)
	}
	if primarySessions("not json") != 0 || primarySessions("{}") != 0 {
		t.Error("no primaries expected")
	}
}

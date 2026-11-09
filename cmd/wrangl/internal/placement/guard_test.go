package placement

import (
	"context"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func f(v float64) *float64 { return &v }

func TestEvaluateGuardHysteresis(t *testing.T) {
	th := Thresholds{Load1: f(6), MemPct: f(85), DiskPct: f(85)}
	var st GuardState
	st = EvaluateGuard(th, st, 1, 50, 87) // one spike: not closed
	if st.Closed || st.Breaches != 1 {
		t.Fatalf("%+v", st)
	}
	st = EvaluateGuard(th, st, 1, 50, 60) // recovered: counter resets
	if st.Closed || st.Breaches != 0 {
		t.Fatalf("%+v", st)
	}
	st = EvaluateGuard(th, st, 1, 50, 87)
	st = EvaluateGuard(th, st, 1, 50, 88) // two in a row: closed
	if !st.Closed || st.Reason != "disk 88% > 85%" {
		t.Fatalf("%+v", st)
	}
	st = EvaluateGuard(th, st, 1, 50, 80) // below the threshold but above 90% of it (76.5): stays closed
	st = EvaluateGuard(th, st, 1, 50, 80)
	if !st.Closed {
		t.Fatalf("reopen needs 90%% of the threshold: %+v", st)
	}
	st = EvaluateGuard(th, st, 1, 50, 70)
	if !st.Closed {
		t.Fatal("one good sample is not enough to reopen")
	}
	st = EvaluateGuard(th, st, 1, 50, 70)
	if st.Closed || st.Oks != 0 {
		t.Fatalf("two samples under 90%% reopen it: %+v", st)
	}
	if st := EvaluateGuard(th, GuardState{}, 7, 90, 90); st.Closed {
		t.Fatal("a single sample never closes")
	}
	// Off by default: no thresholds, never closed.
	for i := 0; i < 5; i++ {
		if EvaluateGuard(Thresholds{}, GuardState{}, 99, 99, 99).Closed {
			t.Fatal("guard off")
		}
	}
	// Several measures name each.
	st = GuardState{}
	st = EvaluateGuard(th, st, 7.5, 90, 10)
	st = EvaluateGuard(th, st, 7.5, 90, 10)
	if st.Reason != "load 7.5 > 6, mem 90% > 85%" {
		t.Fatalf("%q", st.Reason)
	}
}

func TestGuardOnHeartbeatAndSetGuard(t *testing.T) {
	s, d := setup(t)
	ctx := context.Background()
	d.W.Exec(`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('host','','{}',1,1,1)`)
	d.W.Exec(`INSERT INTO runner (name, template_id, state, launched) VALUES ('h', 1, 'idle', 0)`)
	hb := proto.Heartbeat{DiskPercent: 90}
	if _, changed, _ := s.ApplyHeartbeat(ctx, 1, hb); changed {
		t.Fatal("off by default")
	}
	if err := s.SetGuard(ctx, 1, map[string]string{"disk": "85", "mem": "85%"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGuard(ctx, 1, map[string]string{"disk": "abc"}); err == nil {
		t.Fatal("bad threshold accepted")
	}
	s.ApplyHeartbeat(ctx, 1, hb)
	st, changed, err := s.ApplyHeartbeat(ctx, 1, hb)
	if err != nil || !changed || !st.Closed {
		t.Fatalf("%+v %v %v", st, changed, err)
	}
	r, _ := d.RQ.GetRunnerByName(ctx, "h")
	if c, _ := s.RunnerCapacity(ctx, r); !c.GuardClosed || c.GuardReason != "disk 90% > 85%" {
		t.Fatalf("%+v", c)
	}
	// Clearing the thresholds reopens it.
	s.SetGuard(ctx, 1, map[string]string{"disk": "", "mem": ""})
	if c, _ := s.RunnerCapacity(ctx, r); c.GuardClosed {
		t.Fatal("removing the thresholds should reopen the runner")
	}
}

package placement

import (
	"context"
	"database/sql"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
)

func TestBudgetCommittedAndApproaching(t *testing.T) {
	s, d := setup(t)
	ctx := context.Background()
	d.W.Exec(`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('t','','{}',1,1,1)`)
	for i, st := range []string{"idle", "launching", "draining", "terminated"} {
		d.W.Exec(`INSERT INTO runner (name, template_id, state) VALUES (?, 1, ?)`, "r"+string(rune('a'+i)), st)
		d.Q.SetRunnerPrice(ctx, dbPrice(int64(i+1), 0.10))
	}
	d.W.Exec(`INSERT INTO runner (name, template_id, state) VALUES ('host', 1, 'busy')`) // no price: free
	b, _ := s.Budget(ctx)
	if b.LimitUSD != nil || b.Committed < 0.2999 || b.Committed > 0.3001 || b.Approaching() {
		t.Fatalf("launching and draining count, terminated does not: %+v", b)
	}
	if note, _, _ := s.CheckApproaching(ctx); note {
		t.Fatal("no budget, no note")
	}
	if err := s.SetBudget(ctx, "0.50"); err != nil {
		t.Fatal(err)
	}
	if note, _, _ := s.CheckApproaching(ctx); note {
		t.Fatal("60% is below the warning share")
	}
	d.Q.SetRunnerPrice(ctx, dbPrice(1, 0.20)) // 0.40 of 0.50 = 80%
	if note, b, _ := s.CheckApproaching(ctx); !note || !b.Approaching() {
		t.Fatalf("80%% should warn: %+v", b)
	}
	if note, _, _ := s.CheckApproaching(ctx); note {
		t.Fatal("once per crossing")
	}
	d.Q.SetRunnerPrice(ctx, dbPrice(1, 0.14)) // 0.34 = 68%: re-arms
	s.CheckApproaching(ctx)
	d.Q.SetRunnerPrice(ctx, dbPrice(1, 0.20))
	if note, _, _ := s.CheckApproaching(ctx); !note {
		t.Fatal("should warn again after re-arming below 70%")
	}
	if err := s.SetBudget(ctx, "abc"); err == nil {
		t.Fatal("bad budget accepted")
	}
	s.SetBudget(ctx, "none")
	if b, _ := s.Budget(ctx); b.LimitUSD != nil {
		t.Fatal("none should remove the budget")
	}
}

func TestLaunchCommitMac24Hours(t *testing.T) {
	if LaunchCommit("m7g.large", 0.1) != 0.1 {
		t.Fatal("plain instance")
	}
	if got := LaunchCommit("mac2.metal", 0.65); got < 15.59 || got > 15.61 {
		t.Fatalf("a Mac host commits 24 hours: %f", got)
	}
}

func dbPrice(id int64, usd float64) dbgen.SetRunnerPriceParams {
	return dbgen.SetRunnerPriceParams{RunnerID: id, HourlyUsd: sql.NullFloat64{Float64: usd, Valid: true}}
}

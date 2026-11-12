package placer

import "testing"

func f64(v float64) *float64 { return &v }

func run(id int64, n Need) Run { return Run{ID: id, Project: 1, Template: 1, Need: n} }

var big = int64(1 << 40)

func legacyEntry() Entry { return Entry{Template: 1, Cap: Need{big, big, big}, Ceiling: 4} }

func TestPlaceLegacySessionsOnly(t *testing.T) {
	// 9 queued runs, one idle runner with 1 session free, none arriving, room for 5: 1 assigned, 8 left
	// over, so 2 launches of 4 sessions each.
	free := Runner{ID: 7, Claimable: true, Ceiling: i64(4), Active: 3, Cap: Need{big, big, big}}
	var runs []Run
	for i := int64(1); i <= 9; i++ {
		runs = append(runs, run(i, Need{}))
	}
	p := Place(runs, []RunnerIn{{Runner: free, Template: 1}}, []Entry{legacyEntry()}, Limits{Room: map[int64]int64{1: 5}})
	if len(p.Assign) != 1 || p.Assign[0].Runner != 7 || len(p.Launch) != 2 || len(p.Launch[0].Runs) != 4 || len(p.Launch[1].Runs) != 4 || len(p.Unplaceable) != 0 {
		t.Fatalf("%+v", p)
	}
	// Room caps the launches: the rest are blocked, not planned onto imaginary runners.
	p = Place(runs, []RunnerIn{{Runner: free, Template: 1}}, []Entry{legacyEntry()}, Limits{Room: map[int64]int64{1: 1}})
	if len(p.Launch) != 1 || len(p.Unplaceable) != 4 || p.Unplaceable[0].Code != CodeCapacity {
		t.Fatalf("%+v", p)
	}
}

func TestPlaceArrivingRunnerIsReservedBeforeLaunching(t *testing.T) {
	arriving := Runner{ID: 5, Claimable: true, Ceiling: i64(4), Cap: Need{big, big, big}}
	var runs []Run
	for i := int64(1); i <= 5; i++ {
		runs = append(runs, run(i, Need{}))
	}
	p := Place(runs, []RunnerIn{{Runner: arriving, Template: 1, Arriving: true}}, []Entry{legacyEntry()}, Limits{Room: map[int64]int64{1: 9}})
	if len(p.Assign) != 0 || len(p.Wait) != 5 || len(p.Launch) != 1 || len(p.Launch[0].Runs) != 1 {
		t.Fatalf("4 wait for the arriving runner, 1 needs a launch: %+v", p)
	}
}

func TestPlacePicksCheapestPerHostedRun(t *testing.T) {
	small := Entry{Template: 1, Type: "small", Cap: Need{2, 4096, 20}, Ceiling: 8, HourlyUSD: f64(0.04), MinDiskGiB: 10}
	large := Entry{Template: 1, Type: "large", Cap: Need{4, 8192, 20}, Ceiling: 8, HourlyUSD: f64(0.07), MinDiskGiB: 10}
	need := Need{1, 2048, 1}
	// Three 1-vCPU runs: small hosts 2 (0.02 each), large hosts 3 (0.0233 each): small first, then the
	// third run opens another small one.
	p := Place([]Run{run(1, need), run(2, need), run(3, need)}, nil, []Entry{small, large}, Limits{Room: map[int64]int64{1: 9}})
	if len(p.Launch) != 2 || p.Launch[0].Entry.Type != "small" || len(p.Launch[0].Runs) != 2 {
		t.Fatalf("%+v", p.Launch)
	}
	// A cheaper-per-run large box wins when it hosts enough.
	large.HourlyUSD = f64(0.05)
	p = Place([]Run{run(1, need), run(2, need), run(3, need)}, nil, []Entry{small, large}, Limits{Room: map[int64]int64{1: 9}})
	if len(p.Launch) != 1 || p.Launch[0].Entry.Type != "large" || len(p.Launch[0].Runs) != 3 {
		t.Fatalf("%+v", p.Launch)
	}
	// A run that only the large box can hold goes there, whatever the score.
	p = Place([]Run{run(1, Need{4, 6000, 1})}, nil, []Entry{small, large}, Limits{Room: map[int64]int64{1: 9}})
	if len(p.Launch) != 1 || p.Launch[0].Entry.Type != "large" {
		t.Fatalf("%+v", p.Launch)
	}
	// Nothing holds it: too_big, no launch.
	p = Place([]Run{run(1, Need{16, 1, 1})}, nil, []Entry{small, large}, Limits{Room: map[int64]int64{1: 9}})
	if len(p.Launch) != 0 || len(p.Unplaceable) != 1 || p.Unplaceable[0].Code != CodeTooBig {
		t.Fatalf("%+v", p)
	}
}

func TestPlaceVolumeGrowsWithHostedDisk(t *testing.T) {
	e := Entry{Template: 1, Type: "m", Cap: Need{8, 16384, 500}, Ceiling: 8, MinDiskGiB: 30, ReserveDiskGiB: 8}
	p := Place([]Run{run(1, Need{1, 1024, 100}), run(2, Need{1, 1024, 150})}, nil, []Entry{e}, Limits{Room: map[int64]int64{1: 9}})
	if len(p.Launch) != 1 || p.Launch[0].DiskGiB != 258 {
		t.Fatalf("%+v", p.Launch)
	}
}

func TestPlaceHostFirst(t *testing.T) {
	host := Runner{ID: 9, Claimable: true, Ceiling: i64(4), Cap: Need{big, big, big}}
	paid := Runner{ID: 8, Claimable: true, Ceiling: i64(4), Cap: Need{big, big, big}}
	r := run(1, Need{})
	r.Allowed = []int64{2}
	p := Place([]Run{r}, []RunnerIn{{Runner: paid, Template: 1}, {Runner: host, Template: 2}}, []Entry{legacyEntry()}, Limits{})
	if len(p.Assign) != 1 || p.Assign[0].Runner != 9 {
		t.Fatalf("the allowed host is tried first: %+v", p)
	}
}

func TestPlaceBudgetGate(t *testing.T) {
	cheap := Entry{Template: 1, Type: "a.large", Cap: Need{2, 4096, 100}, Ceiling: 4, HourlyUSD: f64(0.20)}
	dear := Entry{Template: 1, Type: "b.xlarge", Cap: Need{4, 8192, 100}, Ceiling: 4, HourlyUSD: f64(0.40)}
	need := Need{1, 1024, 1}
	room := map[int64]int64{1: 9}
	// 0.47 committed of 0.50: a 0.20 launch does not fit; the reason names the cheapest entry.
	p := Place([]Run{run(1, need)}, nil, []Entry{cheap, dear}, Limits{Room: room, Budget: &Budget{Limit: 0.50, Committed: 0.47}})
	if len(p.Launch) != 0 || len(p.Unplaceable) != 1 || p.Unplaceable[0].Code != CodeBudget || p.Unplaceable[0].Type != "a.large" || p.Unplaceable[0].Adds != 0.20 {
		t.Fatalf("%+v", p)
	}
	// Room under the budget: the entry that fits is chosen even if another would score better.
	p = Place([]Run{run(1, need)}, nil, []Entry{cheap, dear}, Limits{Room: room, Budget: &Budget{Limit: 0.50, Committed: 0.25}})
	if len(p.Launch) != 1 || p.Launch[0].Entry.Type != "a.large" {
		t.Fatalf("%+v", p)
	}
	// Launches planned in the same pass count: two 2-vCPU runs need two a.large (0.40) but only one fits in 0.30.
	two := Need{2, 1024, 1}
	p = Place([]Run{run(1, two), run(2, two)}, nil, []Entry{cheap}, Limits{Room: room, Budget: &Budget{Limit: 0.30, Committed: 0}})
	if len(p.Launch) != 1 || len(p.Unplaceable) != 1 || p.Unplaceable[0].Run != 2 {
		t.Fatalf("one tick must not launch past the ceiling: %+v", p)
	}
	// Unknown price fails closed under a budget, and is fine without one.
	unpriced := Entry{Template: 1, Type: "c.large", Cap: Need{2, 4096, 100}, Ceiling: 4}
	p = Place([]Run{run(1, need)}, nil, []Entry{unpriced}, Limits{Room: room, Budget: &Budget{Limit: 5}})
	if len(p.Launch) != 0 || p.Unplaceable[0].Code != CodePrice {
		t.Fatalf("%+v", p)
	}
	p = Place([]Run{run(1, need)}, nil, []Entry{unpriced}, Limits{Room: room})
	if len(p.Launch) != 1 {
		t.Fatalf("%+v", p)
	}
	// A Mac host commits 24 hours.
	mac := Entry{Template: 1, Type: "mac2.metal", Cap: Need{8, 16384, 100}, Ceiling: 4, HourlyUSD: f64(0.65)}
	p = Place([]Run{run(1, need)}, nil, []Entry{mac}, Limits{Room: room, Budget: &Budget{Limit: 10}})
	if len(p.Launch) != 0 || p.Unplaceable[0].Code != CodeBudget {
		t.Fatalf("%+v", p)
	}
}

package placer

import "testing"

func i64(v int64) *int64 { return &v }

func TestFits(t *testing.T) {
	base := Runner{ID: 1, Claimable: true, Cap: Need{4, 8192, 20}, Ceiling: i64(4)}
	for _, c := range []struct {
		name string
		n    Need
		mut  func(*Runner)
		want bool
	}{
		{"empty runner, small need", Need{1, 2048, 5}, nil, true},
		{"exact fit", Need{4, 8192, 20}, nil, true},
		{"ram does not fit", Need{1, 9000, 5}, nil, false},
		{"ram used", Need{1, 4096, 5}, func(r *Runner) { r.Used.RAMMiB = 6000; r.Active = 1 }, false},
		{"zero need ignores a full box", Need{}, func(r *Runner) { r.Used = Need{9, 99999, 99}; r.Active = 1 }, true},
		{"session ceiling reached", Need{}, func(r *Runner) { r.Active = 4 }, false},
		{"no ceiling", Need{}, func(r *Runner) { r.Active = 40; r.Ceiling = nil }, true},
		{"not claimable", Need{}, func(r *Runner) { r.Claimable = false }, false},
	} {
		r := base
		if c.mut != nil {
			c.mut(&r)
		}
		if got := Fits(c.n, r); got != c.want {
			t.Errorf("%s: Fits = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCandidatesOrder(t *testing.T) {
	price := 0.1
	rs := []Runner{
		{ID: 1, Claimable: true, Cap: Need{4, 8192, 20}, HourlyUSD: &price},                                                 // costs money
		{ID: 2, Claimable: true, Cap: Need{4, 8192, 20}},                                                                    // the free host, empty
		{ID: 3, Claimable: true, Cap: Need{4, 8192, 20}, Used: Need{2, 4096, 10}, Active: 2, Busy: true, HourlyUSD: &price}, // fuller than 1
		{ID: 4, Claimable: true, Cap: Need{4, 8192, 20}, Used: Need{4, 8192, 20}, Active: 4, Busy: true},                    // full
	}
	var ids []int64
	for _, c := range Candidates(Need{1, 1024, 2}, rs) {
		ids = append(ids, c.ID)
	}
	if len(ids) != 3 || ids[0] != 2 || ids[1] != 3 || ids[2] != 1 {
		t.Fatalf("want free host, then the fuller paid runner, then the empty one: %v", ids)
	}
}

// With 0/0/0 needs the order is busy first, then the longest idle (what ClaimIdleRunner did).
func TestCandidatesLegacyOrder(t *testing.T) {
	rs := []Runner{
		{ID: 1, Claimable: true, IdleSince: "2026-01-02", Cap: Need{4, 8192, 20}},
		{ID: 2, Claimable: true, IdleSince: "2026-01-01", Cap: Need{8, 16000, 80}},
		{ID: 3, Claimable: true, Busy: true, Active: 1, Cap: Need{2, 4096, 10}},
	}
	c := Candidates(Need{}, rs)
	if len(c) != 3 || c[0].ID != 3 || c[1].ID != 2 || c[2].ID != 1 {
		t.Fatalf("%+v", c)
	}
}

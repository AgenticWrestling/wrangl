package stack

import (
	"errors"
	"math/rand/v2"
	"slices"
	"testing"
)

func TestRankBetweenCases(t *testing.T) {
	cases := []struct{ lo, hi string }{
		{"", ""}, {"V", ""}, {"", "V"}, {"V", "W"}, {"V", "X"}, {"1", ""}, {"", "1"},
		{"", "01"}, {"z", ""}, {"zz", ""}, {"V", "VV"}, {"VV", "W"}, {"a", "a1"},
		{"Vz", "W"}, {"V1", "V2"}, {"0V", "1"}, {"abc", "abd"}, {"y", "z"},
	}
	for _, c := range cases {
		got, err := RankBetween(c.lo, c.hi)
		if err != nil {
			t.Fatalf("(%q,%q): %v", c.lo, c.hi, err)
		}
		if !ValidRank(got) || (c.lo != "" && got <= c.lo) || (c.hi != "" && got >= c.hi) {
			t.Fatalf("(%q,%q) -> %q", c.lo, c.hi, got)
		}
	}
}

func TestRankBetweenRejectsBadBounds(t *testing.T) {
	for _, c := range [][2]string{{"W", "V"}, {"V", "V"}, {"V0", ""}, {"", "V-"}} {
		if _, err := RankBetween(c[0], c[1]); !errors.Is(err, ErrBadRank) {
			t.Fatalf("%q: got %v", c, err)
		}
	}
}

// Random inserts at random positions keep the list strictly sorted and valid.
func TestRankRandomInserts(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	keys := []string{}
	for i := 0; i < 10000; i++ {
		pos := r.IntN(len(keys) + 1)
		lo, hi := "", ""
		if pos > 0 {
			lo = keys[pos-1]
		}
		if pos < len(keys) {
			hi = keys[pos]
		}
		k, err := RankBetween(lo, hi)
		if err != nil {
			t.Fatalf("insert %d between %q and %q: %v", i, lo, hi, err)
		}
		keys = slices.Insert(keys, pos, k)
	}
	for i, k := range keys {
		if !ValidRank(k) {
			t.Fatalf("invalid key %q", k)
		}
		if i > 0 && keys[i-1] >= k {
			t.Fatalf("not strictly sorted at %d: %q >= %q", i, keys[i-1], k)
		}
	}
}

func TestRankAppendPrependGrowth(t *testing.T) {
	last, first := RankFirst(), RankFirst()
	for i := 0; i < 1000; i++ {
		next := RankAfter(last)
		if next <= last || !ValidRank(next) {
			t.Fatalf("append %d: %q after %q", i, next, last)
		}
		last = next
		prev := RankBefore(first)
		if prev >= first || !ValidRank(prev) {
			t.Fatalf("prepend %d: %q before %q", i, prev, first)
		}
		first = prev
	}
	if len(last) > 40 || len(first) > 40 {
		t.Fatalf("keys grew too long: append %d chars, prepend %d chars", len(last), len(first))
	}
	t.Logf("after 1000: append %d chars, prepend %d chars", len(last), len(first))
}

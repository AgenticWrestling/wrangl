package stack

import (
	"errors"
	"sort"
	"strings"
	"testing"
)

func TestSqidRoundTrip(t *testing.T) {
	s, err := NewSqids(DefaultSqidsAlphabet)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for id := int64(1); id <= 5000; id++ {
		q, err := s.Encode(id)
		if err != nil || len(q) < sqidMinLength || seen[q] {
			t.Fatalf("id %d -> %q %v", id, q, err)
		}
		seen[q] = true
		back, err := s.Decode(q)
		if err != nil || back != id {
			t.Fatalf("decode %q -> %d %v", q, back, err)
		}
	}
}

func TestSqidRejects(t *testing.T) {
	s, _ := NewSqids(DefaultSqidsAlphabet)
	if _, err := s.Encode(0); !errors.Is(err, ErrBadSqid) {
		t.Fatal("encode 0")
	}
	for _, bad := range []string{"", "!!!!!!", "a"} {
		if _, err := s.Decode(bad); !errors.Is(err, ErrBadSqid) {
			t.Fatalf("decode %q: %v", bad, err)
		}
	}
	// A multi-id sqid is not a task sqid.
	multi, _ := s.s.Encode([]uint64{1, 2})
	if _, err := s.Decode(multi); !errors.Is(err, ErrBadSqid) {
		t.Fatal("multi-id accepted")
	}
	// Same number, padded to a different length, is a different (non-canonical) spelling.
	long, _ := NewSqidsMin(DefaultSqidsAlphabet, 10)
	alt, _ := long.Encode(42)
	if _, err := s.Decode(alt); !errors.Is(err, ErrBadSqid) {
		t.Fatalf("non-canonical %q accepted", alt)
	}
}

func TestShuffledAlphabet(t *testing.T) {
	a, err := ShuffledAlphabet(nil)
	if err != nil {
		t.Fatal(err)
	}
	sortStr := func(s string) string { b := strings.Split(s, ""); sort.Strings(b); return strings.Join(b, "") }
	if sortStr(a) != sortStr(DefaultSqidsAlphabet) {
		t.Fatal("not a permutation")
	}
	b, _ := ShuffledAlphabet(nil)
	if a == b {
		t.Fatal("two shuffles identical")
	}
	s1, _ := NewSqids(a)
	s2, _ := NewSqids(DefaultSqidsAlphabet)
	q1, _ := s1.Encode(7)
	q2, _ := s2.Encode(7)
	if q1 == q2 {
		t.Fatal("alphabet had no effect")
	}
}

// NewSqidsMin is a test helper building an encoder with a different minimum length.
func NewSqidsMin(alphabet string, min uint8) (*Sqids, error) {
	s, err := sqidsNew(alphabet, min)
	return &Sqids{s: s}, err
}

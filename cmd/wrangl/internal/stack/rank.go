// Package stack holds the work-stack domain: ranks, sqids and (from M4) the rules for
// creating, moving and completing tasks. It is the only writer of task rows.
package stack

import (
	"errors"
	"fmt"
	"strings"
)

// Ranks are fractional-index keys: non-empty strings over rankDigits (base 62, in ASCII
// order so Go string comparison is rank order) that never end in the lowest digit '0'.
// That last rule guarantees a key strictly between any two distinct keys always exists.
const rankDigits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

const (
	rankMin  = '0'
	rankMax  = 'z'
	rankBase = len(rankDigits)
)

// ErrBadRank reports an invalid key or bounds.
var ErrBadRank = errors.New("invalid rank")

func digit(c byte) int { return strings.IndexByte(rankDigits, c) }

// ValidRank reports whether k is a well-formed rank key.
func ValidRank(k string) bool {
	if k == "" || k[len(k)-1] == rankMin {
		return false
	}
	for i := 0; i < len(k); i++ {
		if digit(k[i]) < 0 {
			return false
		}
	}
	return true
}

// RankFirst is the key for the first item in an empty list: the middle digit.
func RankFirst() string { return string(rankDigits[rankBase/2]) }

// RankBetween returns a key strictly between lo and hi. An empty bound is unbounded.
func RankBetween(lo, hi string) (string, error) {
	if (lo != "" && !ValidRank(lo)) || (hi != "" && !ValidRank(hi)) {
		return "", fmt.Errorf("%w: bounds %q, %q", ErrBadRank, lo, hi)
	}
	if lo != "" && hi != "" && lo >= hi {
		return "", fmt.Errorf("%w: %q is not below %q", ErrBadRank, lo, hi)
	}
	switch {
	case lo == "" && hi == "":
		return RankFirst(), nil
	case hi == "":
		return RankAfter(lo), nil
	case lo == "":
		return RankBefore(hi), nil
	}
	return midpoint(lo, hi), nil
}

// RankAfter returns a key above lo. It increments the first digit that can be
// incremented and drops the rest, so repeated appends grow about one character per
// ~36 appends instead of one per ~6 with plain midpoints.
func RankAfter(lo string) string {
	for i := 0; i < len(lo); i++ {
		if lo[i] < rankMax {
			return lo[:i] + string(rankDigits[digit(lo[i])+1])
		}
	}
	return lo + RankFirst() // all 'z'
}

// RankBefore returns a key below hi, mirroring RankAfter: decrement the first digit
// above '1' (so the result never ends in '0') and drop the rest.
func RankBefore(hi string) string {
	for i := 0; i < len(hi); i++ {
		if hi[i] > '1' {
			return hi[:i] + string(rankDigits[digit(hi[i])-1])
		}
	}
	return midpoint("", hi) // only '0'/'1' digits, e.g. "1" -> "0V"
}

// midpoint returns a key strictly between a and b, where a < b, a may be "" (treated as
// all '0') and b may be "" (unbounded above). Both are valid keys or empty.
func midpoint(a, b string) string {
	if b != "" {
		// Shared prefix, padding a with '0'.
		n := 0
		for n < len(b) && at(a, n) == b[n] {
			n++
		}
		if n > 0 {
			return b[:n] + midpoint(tail(a, n), b[n:])
		}
	}
	da := 0
	if a != "" {
		da = digit(a[0])
	}
	db := rankBase
	if b != "" {
		db = digit(b[0])
	}
	if db-da > 1 {
		return string(rankDigits[(da+db)/2])
	}
	// Adjacent first digits.
	if len(b) > 1 {
		return b[:1] // shorter than b so below it; b[0] > a[0] so above a; b[0] != '0'
	}
	return string(rankDigits[da]) + midpoint(tail(a, 1), "")
}

func at(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return rankMin
}

func tail(s string, n int) string {
	if n >= len(s) {
		return ""
	}
	return s[n:]
}

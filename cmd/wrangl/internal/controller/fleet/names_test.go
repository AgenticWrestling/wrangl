package fleet

import (
	"regexp"
	"strings"
	"testing"
)

func TestRandomNameShape(t *testing.T) {
	ok := regexp.MustCompile(`^[a-z0-9]+-[a-z0-9]+-[a-z0-9]+$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		n := randomName()
		if !ok.MatchString(n) || strings.Count(n, "-") != 2 {
			t.Fatalf("bad name %q", n)
		}
		seen[n] = true
	}
	if len(seen) < 150 {
		t.Fatalf("names should be varied: %d distinct of 200", len(seen))
	}
}

func TestNameListsAreClean(t *testing.T) {
	word := regexp.MustCompile(`^[a-z0-9]+$`)
	for _, l := range [][]string{nameMood, nameThing, nameBand} {
		dup := map[string]bool{}
		for _, w := range l {
			if !word.MatchString(w) || dup[w] {
				t.Fatalf("bad or duplicate word %q", w)
			}
			dup[w] = true
		}
	}
}

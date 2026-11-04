package prs

import (
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
)

func TestOutsideResearch(t *testing.T) {
	got := OutsideResearch([]gh.PRFile{{Path: "research/AB-x.md"}, {Path: "README.md"}, {Path: "research/../x"}})
	if len(got) != 2 || got[0] != "README.md" {
		t.Fatalf("%v", got)
	}
}

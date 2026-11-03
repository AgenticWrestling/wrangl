package cli_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	wcli "github.com/nreid/devhost/cmd/wrangl/internal/cli"
)

// Every `wrangl <command> [subcommand]` the skill names must exist, so the skill cannot drift
// away from the CLI.
func TestSkillCommandsExist(t *testing.T) {
	b, err := os.ReadFile("../../../../docs/wrangl/skill/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	root := wcli.NewRoot()
	re := regexp.MustCompile("`wrangl ([a-z][a-z-]*(?: [a-z][a-z-]*)?)")
	seen := 0
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		words := strings.Fields(m[1])
		// Try the longest prefix that is a command; the rest are arguments (PROJECT, SQID are upper-case and not matched).
		var found bool
		for n := len(words); n >= 1 && !found; n-- {
			if c, rest, err := root.Find(words[:n]); err == nil && c != root && len(rest) == 0 {
				found = true
			}
		}
		if !found {
			t.Errorf("the skill names `wrangl %s`, which is not a command", m[1])
		}
		seen++
	}
	if seen < 15 {
		t.Fatalf("only %d commands found in the skill; the pattern is broken", seen)
	}
}

package stacktools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIssueReadToolsForEveryKindAndOnlyReads(t *testing.T) {
	reads := []string{"issue_search", "issue_get", "issue_comments"}
	for _, kind := range []string{"work", "revise", "review", "plan", "user"} {
		ok := Allowed(kind)
		for _, n := range reads {
			if !ok[n] {
				t.Errorf("%s runs should have %s", kind, n)
			}
		}
		for n := range ok {
			if strings.HasPrefix(n, "issue_") && !contains(reads, n) {
				t.Errorf("%s: %s is not a Phase A read tool", kind, n)
			}
		}
	}
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

func TestIssueToolDefinitions(t *testing.T) {
	seen := 0
	for _, tool := range For("work") {
		if !strings.HasPrefix(tool.Name, "issue_") {
			continue
		}
		seen++
		if !strings.Contains(tool.Description, "untrusted-github-issue") || !strings.Contains(strings.ToLower(tool.Description), "never") {
			t.Errorf("%s description must tell the agent the text is untrusted data", tool.Name)
		}
		if !json.Valid(tool.InputSchema) {
			t.Errorf("%s: invalid schema", tool.Name)
		}
	}
	if seen != 3 {
		t.Fatalf("want 3 issue tools, got %d", seen)
	}
}

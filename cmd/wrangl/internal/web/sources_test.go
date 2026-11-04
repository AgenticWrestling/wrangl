package web

import "testing"

func TestSplitSources(t *testing.T) {
	md := "## Question\nq\n## Sources\n1. a\n2. b\n## Assumptions\nx\n"
	body, src := splitSources(md)
	if src != "1. a\n2. b" || body != "## Question\nq\n## Assumptions\nx\n" {
		t.Fatalf("body %q sources %q", body, src)
	}
	body, src = splitSources("## Question\nq\n## Sources\n1. a\n")
	if src != "1. a" || body != "## Question\nq" {
		t.Fatalf("trailing sources: %q %q", body, src)
	}
	if b, s := splitSources("# Title\n```\n## Sources\n```\n"); s != "" || b == "" {
		t.Fatalf("fenced heading must not match: %q %q", b, s)
	}
}

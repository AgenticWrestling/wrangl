package capability

import "testing"

func TestParseAndMatch(t *testing.T) {
	tags := Tags{"arch": "arm64", "tool:go": "1.22.3", "agent:claude": "authed", "net": "vpn", "gpu": "none"}
	for _, c := range []struct {
		req  string
		want bool
	}{
		{"arch=arm64", true}, {"arch=x86_64", false},
		{"agent:claude", true}, {"agent:codex", false}, {"agent:claude=authed", true},
		{"tool:go>=1.22", true}, {"tool:go>=1.23", false}, {"tool:go>=1.9", true}, {"tool:node>=18", false},
		{"net=public|vpn", true}, {"net=public|lan", false},
	} {
		r, err := Parse(c.req)
		if err != nil {
			t.Fatalf("%s: %v", c.req, err)
		}
		if got := r.Holds(tags); got != c.want {
			t.Errorf("%s holds = %v, want %v", c.req, got, c.want)
		}
		if again, _ := Parse(r.String()); again != r {
			t.Errorf("%s does not round-trip: %v vs %v", c.req, again, r)
		}
	}
	for _, bad := range []string{"", "Arch=x", "tool:go>=abc", "a b=c", "k=", "=v"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestMatchSetsAndUntaggedRunners(t *testing.T) {
	a, _ := Parse("arch=arm64")
	b, _ := Parse("gpu")
	tags := Tags{"arch": "arm64"}
	if !Match([]Requirement{a}, tags) || Match([]Requirement{a, b}, tags) {
		t.Fatal("every requirement must hold")
	}
	if !Match(nil, tags) || !Match(nil, nil) {
		t.Fatal("an empty set matches anything")
	}
	if !Match([]Requirement{a, b}, Tags{}) {
		t.Fatal("a runner with no tags is unrestricted")
	}
	if got := (Tags{"b": "2", "a": ""}).Format(); len(got) != 2 || got[0] != "a" || got[1] != "b=2" {
		t.Fatalf("%v", got)
	}
}

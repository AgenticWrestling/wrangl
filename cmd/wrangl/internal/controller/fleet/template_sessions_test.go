package fleet

import "testing"

func TestTemplateMaxSessions(t *testing.T) {
	base := "name: s\nmin: {vcpu: 1, ram: 1GiB, disk: 10GiB}\nmise_toml: \"\"\nagent:\n  start: [\"true\"]\n"
	for in, want := range map[string]int64{"": 4, "max_sessions: 1\n": 1, "max_sessions: 7\n": 7} {
		p, err := ParseTemplate([]byte(base + in))
		if err != nil || p.MaxSessions != want {
			t.Fatalf("%q: %d %v (want %d)", in, p.MaxSessions, err, want)
		}
	}
	if _, err := ParseTemplate([]byte(base + "max_sessions: 0\n")); err == nil {
		t.Fatal("max_sessions 0 must be refused")
	}
}

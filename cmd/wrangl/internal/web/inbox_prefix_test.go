package web

import "testing"

func TestStripTaskPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"Task hIqdqc (#691): LiftClass needs your call?": "LiftClass needs your call?",
		"Task hIqdqc: Which one?":                        "Which one?",
		"Which task should go first?":                    "Which task should go first?",
	} {
		if got := stripTaskPrefix(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

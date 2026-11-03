package stack_test

import (
	"testing"
	"time"

	"github.com/nreid/devhost/cmd/wrangl/internal/stack"
)

func TestSlippage(t *testing.T) {
	day := 24 * time.Hour
	created := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	target := created.Add(10 * day)
	for _, c := range []struct {
		name        string
		now         time.Time
		done, total int64
		finished    bool
		want        string
	}{
		{"no date", created.Add(5 * day), 0, 10, false, "ok"},
		{"on track", created.Add(5 * day), 5, 10, false, ""},
		{"slightly behind is fine", created.Add(5 * day), 4, 10, false, ""},
		{"clearly behind", created.Add(8 * day), 3, 10, false, "at_risk"},
		{"the day itself is on time", target.Add(12 * time.Hour), 8, 10, false, "at_risk"},
		{"past the date", target.Add(2 * day), 9, 10, false, "late"},
		{"finished is never late", target.Add(20 * day), 10, 10, true, ""},
		{"no leaves yet", created.Add(8 * day), 0, 0, false, ""},
	} {
		tg := target
		if c.name == "no date" {
			tg = time.Time{}
		}
		got := stack.Slippage(created, tg, c.now, c.done, c.total, c.finished)
		if c.want == "ok" {
			c.want = ""
		}
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

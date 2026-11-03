package stack

import (
	"context"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
)

// Slippage says whether a container with a target date is on track: "" (fine or no date),
// "at_risk" (clearly behind a straight line from when it was created to its target date) or
// "late" (past the date and not finished). done and total count leaves.
func Slippage(created, target, now time.Time, done, total int64, finished bool) string {
	if finished || target.IsZero() {
		return ""
	}
	if now.After(target.Add(24 * time.Hour)) { // the date itself still counts as on time
		return "late"
	}
	span := target.Sub(created)
	if total == 0 || span <= 0 {
		return ""
	}
	elapsed := float64(now.Sub(created)) / float64(span)
	if elapsed > 1 {
		elapsed = 1
	}
	if float64(done)/float64(total) < elapsed-0.15 {
		return "at_risk"
	}
	return ""
}

// Rollup is a container's progress over its leaves and whether it is slipping against its
// target date.
func (s *Service) Rollup(ctx context.Context, t dbgen.Task) (done, total int64, slip string, err error) {
	p, err := s.DB.RQ.TaskProgress(ctx, t.ID)
	if err != nil {
		return 0, 0, "", err
	}
	var target, created time.Time
	if t.TargetDate.Valid {
		target, _ = time.Parse("2006-01-02", t.TargetDate.String)
	}
	created, _ = time.Parse(time.RFC3339Nano, t.CreatedAt)
	finished := t.State == "done" || t.State == "cancelled"
	return p.Done, p.Total, Slippage(created, target, time.Now().UTC(), p.Done, p.Total, finished), nil
}

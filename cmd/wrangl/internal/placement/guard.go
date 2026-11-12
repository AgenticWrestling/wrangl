package placement

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// Thresholds are a runner's measured limits; nil = that measure is not guarded. All nil (the default)
// means the guard is off.
type Thresholds struct{ Load1, MemPct, DiskPct *float64 }

func (t Thresholds) None() bool { return t.Load1 == nil && t.MemPct == nil && t.DiskPct == nil }

// GuardState is what the guard remembers between heartbeats.
type GuardState struct {
	Closed   bool
	Breaches int // consecutive samples over a threshold
	Oks      int // consecutive samples back under 90% of every threshold
	Reason   string
}

// Samples needed to close, and to reopen, so one spike does not flap a runner.
const (
	GuardCloseAfter  = 2
	GuardReopenAfter = 2
	GuardReopenShare = 0.9 // reopen at 90% of the threshold
)

// EvaluateGuard is the pure rule (ADR 0007 section 7.2): close when a sample is over a threshold for
// GuardCloseAfter consecutive heartbeats; reopen after GuardReopenAfter consecutive samples under 90% of
// every threshold. The guard only closes a runner to NEW assignments.
func EvaluateGuard(th Thresholds, st GuardState, load1, mem, disk float64) GuardState {
	if th.None() {
		return GuardState{}
	}
	var over []string
	under := true
	check := func(name string, thr *float64, v float64, unit string) {
		if thr == nil {
			return
		}
		if v > *thr {
			over = append(over, fmt.Sprintf("%s %s%s > %s%s", name, trim(v), unit, trim(*thr), unit))
		}
		if v > *thr*GuardReopenShare {
			under = false
		}
	}
	check("load", th.Load1, load1, "")
	check("mem", th.MemPercent(), mem, "%")
	check("disk", th.DiskPct, disk, "%")
	if !st.Closed {
		if len(over) == 0 {
			return GuardState{}
		}
		st.Breaches++
		st.Oks = 0
		if st.Breaches >= GuardCloseAfter {
			st.Closed, st.Reason = true, strings.Join(over, ", ")
		}
		return st
	}
	switch {
	case len(over) > 0:
		st.Oks, st.Reason = 0, strings.Join(over, ", ")
	case under:
		st.Oks++
		if st.Oks >= GuardReopenAfter {
			return GuardState{}
		}
	default:
		st.Oks = 0
	}
	return st
}

// MemPercent exists so the three checks read alike.
func (t Thresholds) MemPercent() *float64 { return t.MemPct }

func trim(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func nullF(p *float64) sql.NullFloat64 {
	if p == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *p, Valid: true}
}

func ptr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}

// RunnerGuard returns a runner's thresholds and state.
func (s *Service) RunnerGuard(ctx context.Context, runnerID int64) (Thresholds, GuardState, error) {
	row, err := s.DB.RQ.GetRunnerGuard(ctx, runnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return Thresholds{}, GuardState{}, nil
	} else if err != nil {
		return Thresholds{}, GuardState{}, err
	}
	return Thresholds{Load1: ptr(row.GuardLoad1), MemPct: ptr(row.GuardMemPct), DiskPct: ptr(row.GuardDiskPct)},
		GuardState{Closed: row.ClosedByGuard == 1, Breaches: int(row.GuardBreaches), Oks: int(row.GuardOks), Reason: row.GuardReason}, nil
}

// SetGuard sets a runner's thresholds: load1=6, mem=85, disk=85 (percent); an empty value stops guarding
// that measure. Changing them reopens the runner; the next heartbeats judge it afresh.
func (s *Service) SetGuard(ctx context.Context, runnerID int64, patch map[string]string) error {
	th, _, err := s.RunnerGuard(ctx, runnerID)
	if err != nil {
		return err
	}
	for k, v := range patch {
		var dst **float64
		switch k {
		case "load1", "load":
			dst = &th.Load1
		case "mem":
			dst = &th.MemPct
		case "disk":
			dst = &th.DiskPct
		default:
			return fmt.Errorf("%w: unknown guard %q (load1, mem, disk)", errs.Invalid, k)
		}
		v = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "%"))
		if v == "" {
			*dst = nil
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || (k != "load1" && k != "load" && f > 100) {
			return fmt.Errorf("%w: guard %s %q: want a positive number (percent for mem and disk)", errs.Invalid, k, v)
		}
		*dst = &f
	}
	if err := s.DB.Q.SetRunnerGuardThresholds(ctx, dbgen.SetRunnerGuardThresholdsParams{RunnerID: runnerID,
		GuardLoad1: nullF(th.Load1), GuardMemPct: nullF(th.MemPct), GuardDiskPct: nullF(th.DiskPct)}); err != nil {
		return err
	}
	return s.DB.Q.SetRunnerGuardState(ctx, dbgen.SetRunnerGuardStateParams{RunnerID: runnerID})
}

// ApplyHeartbeat evaluates the guard for one heartbeat and stores the new state. changed is true when the
// runner closed or reopened.
func (s *Service) ApplyHeartbeat(ctx context.Context, runnerID int64, hb proto.Heartbeat) (st GuardState, changed bool, err error) {
	th, cur, err := s.RunnerGuard(ctx, runnerID)
	if err != nil || (th.None() && !cur.Closed) {
		return cur, false, err
	}
	st = EvaluateGuard(th, cur, hb.Load1, hb.MemPercent, hb.DiskPercent)
	if st == cur {
		return st, false, nil
	}
	closed := int64(0)
	if st.Closed {
		closed = 1
	}
	err = s.DB.Q.SetRunnerGuardState(ctx, dbgen.SetRunnerGuardStateParams{ClosedByGuard: closed, GuardBreaches: int64(st.Breaches),
		GuardOks: int64(st.Oks), GuardReason: st.Reason, RunnerID: runnerID})
	return st, st.Closed != cur.Closed, err
}

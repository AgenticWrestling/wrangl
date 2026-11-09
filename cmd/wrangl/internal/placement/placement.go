// Package placement holds the resource need vocabulary of ADR 0009 section 4: three resources
// (vcpu, ram, disk), the global < project < task resolution, the floors, and the rule that agent
// tools may only raise a need.
package placement

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

// Fields of a need set, in display order.
var Fields = []string{"vcpu", "ram", "disk"}

// Floors for values the owner enters (the seeded 0/0/0 global set is exempt).
const (
	FloorVCPU    = 1
	FloorRAMMiB  = 512
	FloorDiskGiB = 1
)

// Need is a need set; nil = not set (inherit).
type Need struct {
	VCPU, RAMMiB, DiskGiB *int64
}

// Resolved is a complete need set with where each field came from.
type Resolved struct {
	VCPU, RAMMiB, DiskGiB int64
	Source                map[string]string // field -> default | project | task
}

// ParseValue reads one field's value: vcpu "2"; ram "512MiB", "2GiB" (bare numbers are MiB);
// disk "5GiB", "500MiB" (bare numbers are GiB, MiB rounds up). It enforces the floor.
func ParseValue(field, s string) (int64, error) {
	s = strings.TrimSpace(s)
	num, unit := s, ""
	if i := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		num, unit = s[:i], strings.ToLower(strings.TrimSpace(s[i:]))
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: %s %q: want a whole number%s", errs.Invalid, field, s, map[string]string{"vcpu": "", "ram": " with MiB or GiB, e.g. 2GiB", "disk": " with GiB or MiB, e.g. 5GiB"}[field])
	}
	var v int64
	switch field {
	case "vcpu":
		if unit != "" {
			return 0, fmt.Errorf("%w: vcpu is a whole number of vCPUs, got %q", errs.Invalid, s)
		}
		v = n
		if v < FloorVCPU {
			return 0, fmt.Errorf("%w: vcpu must be at least %d", errs.Invalid, FloorVCPU)
		}
	case "ram":
		switch unit {
		case "", "mib", "mb", "m":
			v = n
		case "gib", "gb", "g":
			v = n * 1024
		default:
			return 0, fmt.Errorf("%w: ram unit %q: use MiB or GiB", errs.Invalid, unit)
		}
		if v < FloorRAMMiB {
			return 0, fmt.Errorf("%w: ram must be at least %d MiB", errs.Invalid, FloorRAMMiB)
		}
	case "disk":
		switch unit {
		case "", "gib", "gb", "g":
			v = n
		case "mib", "mb", "m":
			v = (n + 1023) / 1024
		default:
			return 0, fmt.Errorf("%w: disk unit %q: use GiB or MiB", errs.Invalid, unit)
		}
		if v < FloorDiskGiB {
			return 0, fmt.Errorf("%w: disk must be at least %d GiB", errs.Invalid, FloorDiskGiB)
		}
	default:
		return 0, fmt.Errorf("%w: unknown resource %q (vcpu, ram, disk)", errs.Invalid, field)
	}
	return v, nil
}

// ParsePatch turns "ram=4GiB" style assignments into a patch; an empty value clears the field.
func ParsePatch(assignments []string) (map[string]string, error) {
	out := map[string]string{}
	for _, a := range assignments {
		k, v, ok := strings.Cut(a, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		if !ok || (k != "vcpu" && k != "ram" && k != "disk") {
			return nil, fmt.Errorf("%w: need %q: want vcpu=N, ram=2GiB or disk=5GiB (empty value clears)", errs.Invalid, a)
		}
		out[k] = v
	}
	return out, nil
}

// Format shows a need value: "2 vCPU", "2 GiB" / "512 MiB", "5 GiB"; "0" sets show as "none".
func Format(field string, v int64) string {
	switch field {
	case "vcpu":
		return fmt.Sprintf("%d vCPU", v)
	case "ram":
		if v%1024 == 0 {
			return fmt.Sprintf("%d GiB", v/1024)
		}
		return fmt.Sprintf("%d MiB", v)
	}
	return fmt.Sprintf("%d GiB", v)
}

// View is the display form.
func (r Resolved) View() (vcpu, ram, disk string) {
	return Format("vcpu", r.VCPU), Format("ram", r.RAMMiB), Format("disk", r.DiskGiB)
}

func (r Resolved) String() string {
	return fmt.Sprintf("%s · %s · %s", Format("vcpu", r.VCPU), Format("ram", r.RAMMiB), Format("disk", r.DiskGiB))
}

func fromRow(n dbgen.OptionalNeed) Need {
	p := func(v sql.NullInt64) *int64 {
		if !v.Valid {
			return nil
		}
		x := v.Int64
		return &x
	}
	return Need{VCPU: p(n.NeedVcpu), RAMMiB: p(n.NeedRamMib), DiskGiB: p(n.NeedDiskGib)}
}

func toRow(n Need) dbgen.OptionalNeed {
	nl := func(v *int64) sql.NullInt64 {
		if v == nil {
			return sql.NullInt64{}
		}
		return sql.NullInt64{Int64: *v, Valid: true}
	}
	return dbgen.OptionalNeed{NeedVcpu: nl(n.VCPU), NeedRamMib: nl(n.RAMMiB), NeedDiskGib: nl(n.DiskGiB)}
}

func (n *Need) field(f string) **int64 {
	switch f {
	case "vcpu":
		return &n.VCPU
	case "ram":
		return &n.RAMMiB
	}
	return &n.DiskGiB
}

// Apply returns n with a patch applied (values parsed and floored; "" clears).
func Apply(n Need, patch map[string]string) (Need, error) {
	for _, f := range Fields {
		s, ok := patch[f]
		if !ok {
			continue
		}
		if strings.TrimSpace(s) == "" {
			*n.field(f) = nil
			continue
		}
		v, err := ParseValue(f, s)
		if err != nil {
			return n, err
		}
		*n.field(f) = &v
	}
	for k := range patch {
		if k != "vcpu" && k != "ram" && k != "disk" {
			return n, fmt.Errorf("%w: unknown resource %q (vcpu, ram, disk)", errs.Invalid, k)
		}
	}
	return n, nil
}

// Service reads and writes the layers.
type Service struct {
	DB *db.DB
	// Now is the clock for time-based policies (nil: the system clock).
	Now func() time.Time
}

// Global returns the global default set.
func (s *Service) Global(ctx context.Context) (Resolved, error) {
	g, err := s.DB.RQ.GetPlacementConfig(ctx)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{VCPU: g.NeedVcpu, RAMMiB: g.NeedRamMib, DiskGiB: g.NeedDiskGib}, nil
}

// SetGlobal changes the global default set; every field given must meet its floor.
func (s *Service) SetGlobal(ctx context.Context, patch map[string]string) (Resolved, error) {
	cur, err := s.Global(ctx)
	if err != nil {
		return cur, err
	}
	for f, v := range patch {
		if strings.TrimSpace(v) == "" {
			return cur, fmt.Errorf("%w: the default for %s cannot be cleared; give a value", errs.Invalid, f)
		}
	}
	n, err := Apply(Need{VCPU: &cur.VCPU, RAMMiB: &cur.RAMMiB, DiskGiB: &cur.DiskGiB}, patch)
	if err != nil {
		return cur, err
	}
	out := Resolved{VCPU: *n.VCPU, RAMMiB: *n.RAMMiB, DiskGiB: *n.DiskGiB}
	err = s.DB.Q.SetPlacementNeeds(ctx, dbgen.SetPlacementNeedsParams{NeedVcpu: out.VCPU, NeedRamMib: out.RAMMiB, NeedDiskGib: out.DiskGiB})
	return out, err
}

// ProjectNeed is the project's own overrides.
func (s *Service) ProjectNeed(ctx context.Context, projectID int64) (Need, error) {
	r, err := s.DB.RQ.GetProjectNeed(ctx, projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return Need{}, nil
	}
	return fromRow(r), err
}

// SetProject applies an owner's patch to a project's overrides.
func (s *Service) SetProject(ctx context.Context, projectID int64, patch map[string]string) (Need, error) {
	cur, err := s.ProjectNeed(ctx, projectID)
	if err != nil {
		return cur, err
	}
	n, err := Apply(cur, patch)
	if err != nil {
		return cur, err
	}
	return n, s.DB.Q.SetProjectNeed(ctx, dbgen.SetProjectNeedParams{ProjectID: projectID, OptionalNeed: toRow(n)})
}

// TaskNeed is the task's own overrides.
func (s *Service) TaskNeed(ctx context.Context, taskID int64) (Need, error) {
	r, err := s.DB.RQ.GetTaskNeed(ctx, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return Need{}, nil
	}
	return fromRow(r), err
}

// Resolve layers global < project < task for a task.
func (s *Service) Resolve(ctx context.Context, projectID, taskID int64) (Resolved, error) {
	return resolve(ctx, s.DB.RQ, projectID, taskID)
}

func resolve(ctx context.Context, q *dbgen.Queries, projectID, taskID int64) (Resolved, error) {
	gc, err := q.GetPlacementConfig(ctx)
	if err != nil {
		return Resolved{}, err
	}
	g := Resolved{VCPU: gc.NeedVcpu, RAMMiB: gc.NeedRamMib, DiskGiB: gc.NeedDiskGib,
		Source: map[string]string{"vcpu": "default", "ram": "default", "disk": "default"}}
	layers := []struct {
		name string
		row  func() (dbgen.OptionalNeed, error)
	}{
		{"project", func() (dbgen.OptionalNeed, error) { return q.GetProjectNeed(ctx, projectID) }},
		{"task", func() (dbgen.OptionalNeed, error) { return q.GetTaskNeed(ctx, taskID) }},
	}
	for _, l := range layers {
		row, err := l.row()
		if errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return g, err
		}
		n := fromRow(row)
		set := func(f string, v *int64, dst *int64) {
			if v != nil {
				*dst, g.Source[f] = *v, l.name
			}
		}
		set("vcpu", n.VCPU, &g.VCPU)
		set("ram", n.RAMMiB, &g.RAMMiB)
		set("disk", n.DiskGiB, &g.DiskGiB)
	}
	return g, nil
}

// Snapshot writes the resolved need set of the run's task onto the run (ADR 0009 section 5). Call it
// where a run is queued, with the same querier (so inside the queueing transaction). From then on
// placement reads only run.need_*: editing a default, project or task affects runs queued afterwards,
// never queued or active ones.
func Snapshot(ctx context.Context, q *dbgen.Queries, run dbgen.Run) error {
	t, err := q.GetTask(ctx, run.TaskID)
	if err != nil {
		return err
	}
	r, err := resolve(ctx, q, t.ProjectID, t.ID)
	if err != nil {
		return err
	}
	src, _ := json.Marshal(r.Source)
	return q.SetRunNeed(ctx, dbgen.SetRunNeedParams{NeedVcpu: r.VCPU, NeedRamMib: r.RAMMiB, NeedDiskGib: r.DiskGiB, NeedSource: string(src), ID: run.ID})
}

// SetTask applies a patch to a task's overrides. From an owner surface it may raise, lower or clear
// a field. With raiseOnly (agent tools) each field must be set and at least the task's current
// effective value, so an agent cannot squeeze its siblings.
func (s *Service) SetTask(ctx context.Context, projectID, taskID int64, patch map[string]string, raiseOnly bool) (Need, error) {
	cur, err := s.TaskNeed(ctx, taskID)
	if err != nil {
		return cur, err
	}
	n, err := Apply(cur, patch)
	if err != nil {
		return cur, err
	}
	if raiseOnly {
		eff, err := s.Resolve(ctx, projectID, taskID)
		if err != nil {
			return cur, err
		}
		for f, v := range map[string]int64{"vcpu": eff.VCPU, "ram": eff.RAMMiB, "disk": eff.DiskGiB} {
			if _, ok := patch[f]; !ok {
				continue
			}
			p := *n.field(f)
			if p == nil || *p < v {
				return cur, fmt.Errorf("%w: an agent may only raise a task's %s need (currently %s)", errs.Forbidden, f, Format(f, v))
			}
		}
	}
	return n, s.DB.Q.SetTaskNeed(ctx, dbgen.SetTaskNeedParams{TaskID: taskID, OptionalNeed: toRow(n)})
}

package placement

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// Reserves a launched runner keeps for the OS, the daemon and caches (ADR 0009 section 6.1).
const (
	ReserveRAMMiB  = 1024
	ReserveDiskGiB = 8
)

// The controller host's declared capacity when nothing explicit is set (ADR 0009 decision 5).
const (
	HostVCPU    = 6
	HostRAMMiB  = 12 * 1024
	HostDiskGiB = 20
)

// HostPrimaryReserveMiB is what each primary Claude session on the controller host holds resident
// (ADR 0011 section 3: an idle one held 290-770 MB), taken out of the host's RAM capacity so run
// placement accounts for it.
const HostPrimaryReserveMiB = 700

// primarySessions counts the primary sessions in a runner's latest heartbeat.
func primarySessions(metricsJSON string) int64 {
	var hb proto.Heartbeat
	if json.Unmarshal([]byte(metricsJSON), &hb) != nil {
		return 0
	}
	var n int64
	for _, c := range hb.Claude {
		if c.Kind == proto.ClaudePrimary {
			n++
		}
	}
	return n
}

// Capacity is what a runner can hold and what its active runs have reserved.
type Capacity struct {
	VCPU, RAMMiB, DiskGiB             int64
	UsedVCPU, UsedRAMMiB, UsedDiskGiB int64
	MaxSessions                       *int64 // optional ceiling
	Explicit                          bool   // some value set by the owner
	GuardClosed                       bool   // closed to new assignments by the measured guard
	GuardReason                       string
}

// DeriveCapacity is the capacity of a runner from its facts: a launched box minus the reserve; an
// adopted box (the controller host, which also runs the controller and the database) the declared host
// default, never more than 75% of its vCPU and RAM or 70% of its disk.
func DeriveCapacity(launched bool, vcpu, ramMiB, diskGiB int64) (cv, cr, cd int64) {
	if launched {
		return vcpu, max(ramMiB-ReserveRAMMiB, 0), max(diskGiB-ReserveDiskGiB, 0)
	}
	return min(HostVCPU, vcpu*3/4), min(HostRAMMiB, ramMiB*3/4), min(HostDiskGiB, diskGiB*7/10)
}

// RunnerCapacity returns the runner's effective capacity (explicit values over derived ones) and usage.
func (s *Service) RunnerCapacity(ctx context.Context, r dbgen.Runner) (Capacity, error) {
	var c Capacity
	c.VCPU, c.RAMMiB, c.DiskGiB = DeriveCapacity(r.Launched == 1, r.Vcpu, r.RamMib, r.DiskGib)
	if r.Launched == 0 { // the host: primary sessions hold RAM the derived default must leave alone
		c.RAMMiB = max(c.RAMMiB-primarySessions(r.LastMetricsJson)*HostPrimaryReserveMiB, 0)
	}
	row, err := s.DB.RQ.GetRunnerCapacity(ctx, r.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return c, err
	}
	if row.CapVcpu.Valid {
		c.VCPU, c.Explicit = row.CapVcpu.Int64, true
	}
	if row.CapRamMib.Valid {
		c.RAMMiB, c.Explicit = row.CapRamMib.Int64, true
	}
	if row.CapDiskGib.Valid {
		c.DiskGiB, c.Explicit = row.CapDiskGib.Int64, true
	}
	if row.MaxSessions.Valid {
		m := row.MaxSessions.Int64
		c.MaxSessions = &m
	}
	if _, st, err := s.RunnerGuard(ctx, r.ID); err == nil {
		c.GuardClosed, c.GuardReason = st.Closed, st.Reason
	}
	u, err := s.DB.RQ.SumActiveNeeds(ctx, dbgen.SumActiveNeedsParams{RunnerID: r.ID, NeedsInputCutoff: s.NeedsInputCutoff(ctx)})
	if err != nil {
		return c, err
	}
	c.UsedVCPU, c.UsedRAMMiB, c.UsedDiskGiB = u.Vcpu, u.RamMib, u.DiskGib
	return c, nil
}

// SetRunnerCapacity applies an owner's patch ("vcpu": "6", "ram": "12GiB"; an empty value goes back to
// deriving that field from the runner's facts).
func (s *Service) SetRunnerCapacity(ctx context.Context, runnerID int64, patch map[string]string) error {
	row, err := s.DB.RQ.GetRunnerCapacity(ctx, runnerID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	cur := dbgen.SetRunnerCapacityParams{RunnerID: runnerID, CapVcpu: row.CapVcpu, CapRamMib: row.CapRamMib, CapDiskGib: row.CapDiskGib}
	for f, v := range patch {
		var dst *sql.NullInt64
		switch f {
		case "vcpu":
			dst = &cur.CapVcpu
		case "ram":
			dst = &cur.CapRamMib
		case "disk":
			dst = &cur.CapDiskGib
		default:
			return fmt.Errorf("%w: unknown resource %q (vcpu, ram, disk)", errs.Invalid, f)
		}
		if strings.TrimSpace(v) == "" {
			*dst = sql.NullInt64{}
			continue
		}
		n, err := ParseValue(f, v)
		if err != nil {
			return err
		}
		*dst = sql.NullInt64{Int64: n, Valid: true}
	}
	return s.DB.Q.SetRunnerCapacity(ctx, cur)
}

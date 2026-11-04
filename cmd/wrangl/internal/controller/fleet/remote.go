package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/dbtime"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/provision"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// Remote runners (docs/wrangl/adr/0002). A remote template describes a TARGET machine that runs
// no wrangl binary. The controller launches it, probes it, reaps it when idle and fails the runs
// using it if it disappears; the agent runs on a driver session of connection.driver_template.

// ProbeFailuresToOffline consecutive failed probes mark a target offline.
const ProbeFailuresToOffline = 3

// OfflineGrace is how long a target may stay offline with sessions using it before they are
// failed as target_lost.
var OfflineGrace = 2 * time.Minute

// syncMode stores a template file's mode and connection block.
func (s *Service) syncMode(ctx context.Context, t dbgen.RunnerTemplate, yamlBody []byte) error {
	mode, conn, err := TemplateMode(yamlBody)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	tc := dbgen.TemplateConnection{TemplateID: t.ID, Mode: mode, ConnectionJson: "{}"}
	if conn != nil {
		d, err := s.DB.RQ.GetTemplateByName(ctx, conn.DriverTemplate)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: connection.driver_template %q does not exist", ErrInvalid, conn.DriverTemplate)
		} else if err != nil {
			return err
		}
		if d.ID == t.ID {
			return fmt.Errorf("%w: a remote template cannot be its own driver", ErrInvalid)
		}
		if dc, _ := s.DB.RQ.GetTemplateConnection(ctx, d.ID); dc.Mode == "remote" {
			return fmt.Errorf("%w: driver template %q is itself remote", ErrInvalid, d.Name)
		}
		tc.DriverTemplateID = sql.NullInt64{Int64: d.ID, Valid: true}
		b, _ := json.Marshal(conn)
		tc.ConnectionJson = string(b)
	}
	return s.DB.Q.SetTemplateConnection(ctx, tc)
}

// Connection returns a template's mode and (for remote) its connection block.
func (s *Service) Connection(ctx context.Context, templateID int64) (mode string, conn proto.Connection, driver int64, err error) {
	tc, err := s.DB.RQ.GetTemplateConnection(ctx, templateID)
	if err != nil {
		return "", conn, 0, err
	}
	json.Unmarshal([]byte(tc.ConnectionJson), &conn)
	return tc.Mode, conn, tc.DriverTemplateID.Int64, nil
}

// CostWarning explains billing the owner should know before a target is launched; "" when the
// template has nothing unusual. EC2 Mac instances run on Dedicated Hosts that bill for a 24-hour
// minimum, so a short run costs a full day and releasing early saves nothing.
func CostWarning(t dbgen.RunnerTemplate) string {
	var types []string
	json.Unmarshal([]byte(t.InstanceTypes), &types)
	for _, it := range types {
		if strings.HasPrefix(it, "mac") {
			return fmt.Sprintf("%s is an EC2 Mac (%s): a Dedicated Host with a 24-hour minimum allocation. You pay for at least 24 hours of the host even if the run takes minutes, and stopping it earlier saves nothing (see EC2 Dedicated Host pricing for the mac family)", t.Name, it)
		}
	}
	return ""
}

// LaunchTarget launches the machine of a remote template and records it. There is no user-data
// and no enrolment: the controller owns it. project is the project whose demand caused it (0 = none).
func (s *Service) LaunchTarget(ctx context.Context, template string, project int64) (dbgen.RemoteTarget, error) {
	var zero dbgen.RemoteTarget
	if s.Prov == nil {
		return zero, ErrNoAWS
	}
	t, err := s.Template(ctx, template)
	if err != nil {
		return zero, err
	}
	mode, _, _, err := s.Connection(ctx, t.ID)
	if err != nil {
		return zero, err
	}
	if mode != "remote" {
		return zero, fmt.Errorf("%w: template %q is not remote", ErrInvalid, template)
	}
	var types []string
	json.Unmarshal([]byte(t.InstanceTypes), &types)
	name := s.newName(ctx)
	inst, err := s.Prov.Launch(ctx, provision.Spec{
		Runner: name, Template: t.Name, Arch: t.Arch,
		MinVCPU: int(t.MinVcpu), MinRAMMiB: int(t.MinRamMib), DiskGiB: int(t.MinDiskGib),
		InstanceTypes: types, AMI: t.Ami, SubnetID: s.SubnetID, SecurityGroupID: s.SecurityGroupID,
		InstanceProfile: s.InstanceProfile,
	})
	if err != nil {
		return zero, err
	}
	g, err := s.DB.Q.CreateRemoteTarget(ctx, dbgen.CreateRemoteTargetParams{
		Name: name, TemplateID: t.ID, InstanceID: nullStr(inst.ID), Host: inst.PrivateIP,
		LaunchedForProject: sql.NullInt64{Int64: project, Valid: project != 0}})
	if err != nil {
		s.Log.Error("recording remote target failed; terminating instance", "instance", inst.ID, "err", err)
		_ = s.Prov.Terminate(context.WithoutCancel(ctx), inst.ID)
		return zero, err
	}
	if w := CostWarning(t); w != "" {
		s.Log.Warn("cost warning on launch", "target", name, "warning", w)
		s.note(ctx, "runner", "warn", name, "Cost warning: "+t.Name, w)
	}
	s.Log.Info("launched remote target", "target", name, "instance", inst.ID, "type", inst.Type)
	s.note(ctx, "runner", "info", name, "Remote target launched", fmt.Sprintf("%s · %s · template %s", inst.Type, inst.ID, t.Name))
	s.Bus.Runner(name)
	return g, nil
}

// probeTarget checks a target is alive: the instance is running (EC2) and, when it is reached
// directly, that its port answers. Reached through SSM, the driver's tunnel is the live check.
func (s *Service) probeTarget(ctx context.Context, g dbgen.RemoteTarget, conn proto.Connection) error {
	if s.ProbeTarget != nil {
		return s.ProbeTarget(ctx, g, conn)
	}
	if s.Prov == nil {
		return ErrNoAWS
	}
	if !g.InstanceID.Valid {
		return fmt.Errorf("no instance")
	}
	inst, err := s.Prov.Describe(ctx, g.InstanceID.String)
	if err != nil {
		return err
	}
	if inst == nil || inst.State == "terminated" || inst.State == "shutting-down" {
		return errTargetGone
	}
	if inst.State != "running" {
		return fmt.Errorf("instance is %s", inst.State)
	}
	if conn.Via == "direct" && g.Host != "" {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(g.Host, strconv.Itoa(conn.Port)), 5*time.Second)
		if err != nil {
			return err
		}
		c.Close()
	}
	return nil
}

var errTargetGone = errors.New("instance is gone")

// ReconcileTargets is the remote runners' heartbeat, run on the fleet loop: probe each target,
// move it between launching/ready/offline/terminated, start and stop its idle clock from the runs
// using it, reap it when idle past the template's timeout, and fail the runs of a target that is
// gone as target_lost.
func (s *Service) ReconcileTargets(ctx context.Context) error {
	targets, err := s.DB.RQ.ListRemoteTargets(ctx, false)
	if err != nil {
		return err
	}
	var errs []error
	for _, g := range targets {
		if err := s.reconcileTarget(ctx, g); err != nil {
			errs = append(errs, fmt.Errorf("target %s: %w", g.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) reconcileTarget(ctx context.Context, g dbgen.RemoteTarget) error {
	t, err := s.DB.RQ.GetTemplate(ctx, g.TemplateID)
	if err != nil {
		return err
	}
	_, conn, _, err := s.Connection(ctx, g.TemplateID)
	if err != nil {
		return err
	}
	now := ts(s.now())
	sessions, _ := s.DB.RQ.CountTargetSessions(ctx, g.ID)
	perr := s.probeTarget(ctx, g, conn)
	set := func(state string, fails int64, msg string, idle, off sql.NullString) error {
		defer s.Bus.Runner(g.Name)
		return s.DB.Q.SetRemoteTargetState(ctx, dbgen.SetRemoteTargetStateParams{State: state, ProbeFailures: fails, LastError: msg, Now: now, IdleSince: idle, OfflineSince: off, ID: g.ID})
	}
	switch {
	case errors.Is(perr, errTargetGone):
		if err := s.DB.Q.MarkRemoteTargetTerminated(ctx, g.ID, now); err != nil {
			return err
		}
		s.note(ctx, "runner", "error", g.Name, "Remote target is gone", "its instance no longer exists")
		s.failTargetRuns(ctx, g, "target lost: the instance is gone")
		s.Bus.Runner(g.Name)
		return nil
	case perr != nil:
		fails := g.ProbeFailures + 1
		if fails < ProbeFailuresToOffline && g.State != "offline" {
			return set(g.State, fails, perr.Error(), g.IdleSince, g.OfflineSince)
		}
		off := g.OfflineSince
		if g.State != "offline" {
			off = sql.NullString{String: now, Valid: true}
			s.note(ctx, "runner", "warn", g.Name, "Remote target unreachable", perr.Error())
		}
		if err := set("offline", fails, perr.Error(), sql.NullString{}, off); err != nil {
			return err
		}
		if off.Valid && sessions > 0 {
			if since, e := time.Parse(dbtime.Layout, off.String); e == nil && s.now().Sub(since) > OfflineGrace {
				s.failTargetRuns(ctx, g, "target lost: unreachable for "+OfflineGrace.String())
			}
		}
		return nil
	}
	// healthy
	if g.State == "draining" {
		if sessions == 0 {
			return s.terminateTarget(ctx, g, "drained")
		}
		return nil
	}
	idle := g.IdleSince
	switch {
	case sessions > 0:
		idle = sql.NullString{}
	case !idle.Valid || g.State != "ready":
		idle = sql.NullString{String: now, Valid: true}
	}
	if err := set("ready", 0, "", idle, sql.NullString{}); err != nil {
		return err
	}
	if g.State == "launching" {
		s.note(ctx, "runner", "info", g.Name, "Remote target ready", "")
	}
	if idle.Valid && sessions == 0 {
		if since, e := time.Parse(dbtime.Layout, idle.String); e == nil && s.now().Sub(since) >= time.Duration(t.IdleTimeoutS)*time.Second {
			return s.terminateTarget(ctx, g, fmt.Sprintf("idle for over %s", time.Duration(t.IdleTimeoutS)*time.Second))
		}
	}
	return nil
}

func (s *Service) terminateTarget(ctx context.Context, g dbgen.RemoteTarget, reason string) error {
	if s.Prov == nil {
		return ErrNoAWS
	}
	if g.InstanceID.Valid {
		if err := s.Prov.Terminate(ctx, g.InstanceID.String); err != nil {
			return err
		}
	}
	s.Log.Info("terminated remote target", "target", g.Name, "reason", reason)
	s.note(ctx, "runner", "info", g.Name, "Remote target terminated", reason)
	defer s.Bus.Runner(g.Name)
	return s.DB.Q.MarkRemoteTargetTerminated(ctx, g.ID, ts(s.now()))
}

// TerminateTarget stops a remote target now (draining it first would wait for sessions).
func (s *Service) TerminateTarget(ctx context.Context, name string) error {
	g, err := s.DB.RQ.GetRemoteTargetByName(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: remote target %q", ErrNotFound, name)
	} else if err != nil {
		return err
	}
	if g.State == "terminated" {
		return nil
	}
	s.failTargetRuns(ctx, g, "target lost: terminated by the owner")
	return s.terminateTarget(ctx, g, "requested")
}

func (s *Service) failTargetRuns(ctx context.Context, g dbgen.RemoteTarget, reason string) {
	ids, _ := s.DB.RQ.ActiveRunsOnTarget(ctx, g.ID)
	for _, id := range ids {
		if s.OnTargetLost != nil {
			if err := s.OnTargetLost(ctx, id, reason); err != nil {
				s.Log.Error("failing run on lost target", "run", id, "err", err)
			}
		}
	}
}

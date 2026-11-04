package fleet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

const macYAML = `
name: mac
arch: arm64
min: {vcpu: 2, ram: 8GiB, disk: 40GiB}
max_runners: 2
max_sessions: 2
idle_timeout: 30m
mode: remote
connection:
  driver_template: go-claude
  protocol: ssh
  user: ec2-user
  secret: mac_ssh_key
`

func remoteEnv(t *testing.T) (*env, dbgen.RunnerTemplate) {
	t.Helper()
	e := newEnv(t)
	tp, err := e.s.CreateTemplate(context.Background(), []byte(macYAML))
	if err != nil {
		t.Fatal(err)
	}
	return e, tp
}

func TestRemoteTemplateParsing(t *testing.T) {
	e, tp := remoteEnv(t)
	ctx := context.Background()
	mode, conn, driver, err := e.s.Connection(ctx, tp.ID)
	if err != nil || mode != "remote" || conn.Protocol != "ssh" || conn.Port != 22 || conn.Via != "ssm" || driver == 0 {
		t.Fatalf("connection: %v %v %v %d", mode, conn, err, driver)
	}
	if m, _, _, _ := e.s.Connection(ctx, 1); m != "local" {
		t.Fatalf("go-claude should be local, got %s", m)
	}
	for name, y := range map[string]string{
		"no driver":      strings.Replace(macYAML, "driver_template: go-claude", "driver_template: nope", 1),
		"no connection":  "name: x\narch: arm64\nmin: {vcpu: 1, ram: 1GiB, disk: 10GiB}\nmode: remote\n",
		"bad protocol":   strings.Replace(macYAML, "protocol: ssh", "protocol: telnet", 1),
		"ssh needs key":  strings.Replace(macYAML, "  secret: mac_ssh_key\n", "", 1),
		"connection too": strings.Replace(macYAML, "mode: remote", "mode: local", 1),
	} {
		y = strings.Replace(y, "name: mac", "name: bad-"+strings.ReplaceAll(name, " ", "-"), 1)
		if _, err := e.s.CreateTemplate(ctx, []byte(y)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// A target is launched without enrolment, becomes ready on a good probe, is kept while sessions
// use it, reaped when idle past the timeout, and the runs on a vanished target fail as target_lost.
func TestRemoteTargetLifecycle(t *testing.T) {
	e, tp := remoteEnv(t)
	ctx := context.Background()
	healthy := true
	gone := false
	e.s.ProbeTarget = func(context.Context, dbgen.RemoteTarget, proto.Connection) error {
		switch {
		case gone:
			return errTargetGone
		case !healthy:
			return errors.New("connection refused")
		}
		return nil
	}
	var lost []string
	e.s.OnTargetLost = func(_ context.Context, run int64, reason string) error { lost = append(lost, reason); return nil }

	g, err := e.s.LaunchTarget(ctx, "mac", 0)
	if err != nil || g.State != "launching" || !g.InstanceID.Valid {
		t.Fatalf("launch: %+v %v", g, err)
	}
	if r, _ := e.s.DB.RQ.ListRunners(ctx, 0); len(r) != 0 {
		t.Fatalf("a remote target must not create an enrolling runner: %v", r)
	}
	if err := e.s.ReconcileTargets(ctx); err != nil {
		t.Fatal(err)
	}
	g, _ = e.s.DB.RQ.GetRemoteTarget(ctx, g.ID)
	if g.State != "ready" || !g.IdleSince.Valid {
		t.Fatalf("want ready and idle, got %s idle=%v", g.State, g.IdleSince)
	}
	// ClaimTarget hands out at most max_sessions (2) sessions
	exec := func(q string) { e.s.DB.W.Exec(q) }
	exec(`INSERT INTO project (slug, name, default_template_id) VALUES ('p','P',1)`)
	for i := 1; i <= 3; i++ {
		exec(`INSERT INTO task (project_id, rank, title) VALUES (1, 'V00` + string(rune('0'+i)) + `', 't')`)
		exec(`INSERT INTO run (task_id, state, template_id) VALUES (` + string(rune('0'+i)) + `, 'running', ` + "2" + `)`)
	}
	for i := int64(1); i <= 2; i++ {
		if _, err := e.s.DB.RQ.ClaimTarget(ctx, tp.ID); err != nil {
			t.Fatalf("slot %d should be free: %v", i, err)
		}
		e.s.DB.Q.BindRunTarget(ctx, i, g.ID)
	}
	if _, err := e.s.DB.RQ.ClaimTarget(ctx, tp.ID); err == nil {
		t.Fatal("max_sessions=2: a third session must not get the target")
	}
	// busy: not idle, never reaped
	e.clk.Advance(3 * time.Hour)
	e.s.ReconcileTargets(ctx)
	if g, _ = e.s.DB.RQ.GetRemoteTarget(ctx, g.ID); g.State != "ready" || g.IdleSince.Valid {
		t.Fatalf("busy target: %s idle=%v", g.State, g.IdleSince)
	}
	// sessions end -> idle clock starts -> reaped after idle_timeout
	exec(`UPDATE run SET state = 'succeeded'`)
	e.s.ReconcileTargets(ctx)
	e.clk.Advance(29 * time.Minute)
	e.s.ReconcileTargets(ctx)
	if g, _ = e.s.DB.RQ.GetRemoteTarget(ctx, g.ID); g.State != "ready" {
		t.Fatalf("reaped early: %s", g.State)
	}
	e.clk.Advance(2 * time.Minute)
	e.s.ReconcileTargets(ctx)
	if g, _ = e.s.DB.RQ.GetRemoteTarget(ctx, g.ID); g.State != "terminated" || e.ec2.State(g.InstanceID.String) != "terminated" {
		t.Fatalf("not reaped: %s / %s", g.State, e.ec2.State(g.InstanceID.String))
	}

	// a second target: probe failures -> offline; sessions on it are failed after the grace; gone -> terminated
	g2, _ := e.s.LaunchTarget(ctx, "mac", 0)
	e.s.ReconcileTargets(ctx)
	exec(`UPDATE run SET state = 'running' WHERE id = 3`)
	e.s.DB.Q.BindRunTarget(ctx, 3, g2.ID)
	healthy = false
	for i := 0; i < ProbeFailuresToOffline; i++ {
		e.s.ReconcileTargets(ctx)
	}
	if g2, _ = e.s.DB.RQ.GetRemoteTarget(ctx, g2.ID); g2.State != "offline" {
		t.Fatalf("want offline after %d failures, got %s", ProbeFailuresToOffline, g2.State)
	}
	if len(lost) != 0 {
		t.Fatal("runs must survive a blip")
	}
	e.clk.Advance(OfflineGrace + time.Second)
	e.s.ReconcileTargets(ctx)
	if len(lost) != 1 || !strings.Contains(lost[0], "target lost") {
		t.Fatalf("run should be failed as target lost after the grace: %v", lost)
	}
	healthy, lost = true, nil
	e.s.ReconcileTargets(ctx) // recovers
	if g2, _ = e.s.DB.RQ.GetRemoteTarget(ctx, g2.ID); g2.State != "ready" {
		t.Fatalf("want ready after recovery, got %s", g2.State)
	}
	gone = true
	e.s.ReconcileTargets(ctx)
	if g2, _ = e.s.DB.RQ.GetRemoteTarget(ctx, g2.ID); g2.State != "terminated" || len(lost) != 1 {
		t.Fatalf("gone target: %s, lost %v", g2.State, lost)
	}
}

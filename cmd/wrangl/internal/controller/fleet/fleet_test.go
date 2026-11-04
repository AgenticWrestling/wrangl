package fleet

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nkeys"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/provision"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

const tmplYAML = `
name: go-claude
arch: arm64
min: { vcpu: 4, ram: 16GiB, disk: 100GiB }
instance_types: [m7g.xlarge]
mise_toml: |
  [tools]
  go = "1.26"
agent:
  drive: headless
  start: ["claude", "-p", "{{.Prompt}}"]
`

// clock is a settable fake clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	s   *Service
	ec2 *provision.Fake
	clk *clock
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(dir, "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	keys, err := natsd.EnsureKeys(filepath.Join(dir, "nats"))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "wrangl-linux-aarch64"), []byte("fake runner binary"), 0o755)
	f := provision.NewFake()
	clk := &clock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	s := &Service{
		DB: d, Keys: keys, NATSCA: []byte("CA"), NATSURLs: []string{"tls://10.0.1.5:4222"},
		Prov: &provision.Provisioner{EC2: f, SSM: f, ControllerID: "ctl-test"}, ControllerID: "ctl-test",
		EnrollURL: "http://10.0.1.5:4223", SubnetID: "subnet-1", SecurityGroupID: "sg-1", InstanceProfile: "wrangl-runner",
		BinDir: bin, Now: clk.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if _, err := s.CreateTemplate(context.Background(), []byte(tmplYAML)); err != nil {
		t.Fatal(err)
	}
	return &env{s: s, ec2: f, clk: clk}
}

func userKey(t *testing.T) string {
	kp, _ := nkeys.CreateUser()
	p, _ := kp.PublicKey()
	return p
}

var bigFacts = proto.Facts{Arch: "aarch64", VCPU: 4, RAMMiB: 15800, DiskGiB: 98} // measured < nominal

// launchAndEnroll launches via the fake EC2, digs the token out of the user-data and
// enrols from the instance's IP, like a real instance would.
func (e *env) launchAndEnroll(t *testing.T) (runnerName, instanceID string, resp *proto.EnrollResponse) {
	t.Helper()
	ctx := context.Background()
	r, err := e.s.Launch(ctx, "go-claude")
	if err != nil {
		t.Fatal(err)
	}
	tok := tokenFromUserData(t, e.ec2)
	resp, err = e.s.Enroll(ctx, proto.EnrollRequest{Token: tok, NKeyPublic: userKey(t), InstanceID: r.InstanceID.String, Facts: bigFacts}, r.PrivateIp)
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	return r.Name, r.InstanceID.String, resp
}

func tokenFromUserData(t *testing.T, f *provision.Fake) string {
	t.Helper()
	ud := decodeUD(t, *f.Runs[len(f.Runs)-1].UserData)
	i := strings.Index(ud, "--token \"")
	if i < 0 {
		t.Fatalf("no token in user-data:\n%s", ud)
	}
	rest := ud[i+len("--token \""):]
	return rest[:strings.Index(rest, "\"")]
}

func TestParseTemplate(t *testing.T) {
	p, err := ParseTemplate([]byte(tmplYAML))
	if err != nil {
		t.Fatal(err)
	}
	if p.MinRamMib != 16384 || p.MinDiskGib != 100 || p.IdleTimeoutS != 1800 || p.MaxRunners != 1 ||
		!strings.Contains(p.Ami, "arm64") || !strings.Contains(p.AgentJson, `"drive":"headless"`) {
		t.Fatalf("%+v", p)
	}
	for _, bad := range []string{
		"name: Bad_Name\nmin: {vcpu: 1, ram: 1GiB, disk: 8GiB}",
		"name: x\narch: sparc\nmin: {vcpu: 1, ram: 1GiB, disk: 8GiB}",
		"name: x\nmin: {vcpu: 0, ram: 1GiB, disk: 8GiB}",
		"name: x\nmin: {vcpu: 1, ram: 1PiB, disk: 8GiB}",
		"name: x\nmin: {vcpu: 1, ram: 1GiB, disk: 8GiB}\nidle_timeout: 5s",
		"name: x\nmin: {vcpu: 1, ram: 1GiB, disk: 8GiB}\nunknown_key: 1",
	} {
		if _, err := ParseTemplate([]byte(bad)); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
}

func TestLaunchEnrolHeartbeatReap(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	name, inst, resp := e.launchAndEnroll(t)
	if resp.UserJWT == "" || resp.NATSCA != "CA" || resp.Name != name {
		t.Fatalf("response %+v", resp)
	}
	r, _ := e.s.DB.RQ.GetRunnerByName(ctx, name)
	if r.State != "enrolling" || !r.NkeyPublic.Valid {
		t.Fatalf("after enrol: %s", r.State)
	}
	// First heartbeat -> idle.
	if err := e.s.Heartbeat(ctx, natsd.RunnerHeartbeatSubject(resp.RunnerID), []byte(`{"cpu_percent":3}`)); err != nil {
		t.Fatal(err)
	}
	if r, _ = e.s.DB.RQ.GetRunnerByName(ctx, name); r.State != "idle" {
		t.Fatalf("after heartbeat: %s", r.State)
	}
	// 29 minutes idle (still heartbeating): not reaped.
	for i := 0; i < 29; i++ {
		e.clk.Advance(time.Minute)
		e.s.Heartbeat(ctx, natsd.RunnerHeartbeatSubject(resp.RunnerID), []byte(`{}`))
		if reaped, err := e.s.Reap(ctx); err != nil || len(reaped) != 0 {
			t.Fatalf("reaped early at %d min: %v %v", i+1, reaped, err)
		}
	}
	// 30 minutes: reaped and the instance terminated.
	e.clk.Advance(time.Minute)
	e.s.Heartbeat(ctx, natsd.RunnerHeartbeatSubject(resp.RunnerID), []byte(`{}`))
	reaped, err := e.s.Reap(ctx)
	if err != nil || len(reaped) != 1 || reaped[0] != name {
		t.Fatalf("reap at 30 min: %v %v", reaped, err)
	}
	if e.ec2.State(inst) != "terminated" {
		t.Fatal("instance not terminated")
	}
	if r, _ = e.s.DB.RQ.GetRunnerByName(ctx, name); r.State != "terminated" {
		t.Fatalf("runner state %s", r.State)
	}
	// Spin-up, enrol and spin-down all land on the Logs page.
	ents, _ := e.s.DB.RQ.ListLogEntries(ctx, 50)
	got := map[string]bool{}
	for _, en := range ents {
		if en.Subject == name {
			got[en.Title] = true
		}
	}
	for _, want := range []string{"Runner launched", "Runner enrolled", "Runner terminated"} {
		if !got[want] {
			t.Errorf("no %q log entry for %s: %v", want, name, got)
		}
	}
	// Heartbeats from a terminated runner are ignored, not resurrecting it.
	e.s.Heartbeat(ctx, natsd.RunnerHeartbeatSubject(resp.RunnerID), []byte(`{}`))
	if r, _ = e.s.DB.RQ.GetRunnerByName(ctx, name); r.State != "terminated" {
		t.Fatal("terminated runner resurrected by heartbeat")
	}
}

func TestBusyRunnerIsNotReaped(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	name, inst, resp := e.launchAndEnroll(t)
	e.s.Heartbeat(ctx, natsd.RunnerHeartbeatSubject(resp.RunnerID), []byte(`{}`))
	r, _ := e.s.DB.RQ.GetRunnerByName(ctx, name)
	e.s.DB.W.Exec(`UPDATE runner SET state = 'busy' WHERE id = ?`, r.ID)
	for i := 0; i < 120; i++ {
		e.clk.Advance(time.Minute)
		e.s.Heartbeat(ctx, natsd.RunnerHeartbeatSubject(resp.RunnerID), []byte(`{}`))
		e.s.Reap(ctx)
	}
	if e.ec2.State(inst) == "terminated" {
		t.Fatal("busy runner reaped")
	}
}

func TestNetworkBlipDoesNotResetIdleClock(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	name, inst, resp := e.launchAndEnroll(t)
	hb := func() { e.s.Heartbeat(ctx, natsd.RunnerHeartbeatSubject(resp.RunnerID), []byte(`{}`)) }
	hb() // idle from t0
	e.clk.Advance(61 * time.Second)
	e.s.SweepOffline(ctx)
	if r, _ := e.s.DB.RQ.GetRunnerByName(ctx, name); r.State != "offline" {
		t.Fatalf("want offline, got %s", r.State)
	}
	hb() // back after a blip
	if r, _ := e.s.DB.RQ.GetRunnerByName(ctx, name); r.State != "idle" {
		t.Fatalf("want idle after return, got %s", r.State)
	}
	// Keep heartbeating; the 30 minutes count from t0, not from the return.
	for e.clk.Now().Before(time.Date(2026, 10, 31, 12, 29, 30, 0, time.UTC)) {
		e.clk.Advance(30 * time.Second)
		hb()
		if reaped, _ := e.s.Reap(ctx); len(reaped) != 0 {
			t.Fatalf("reaped early at %s", e.clk.Now())
		}
	}
	e.clk.Advance(30 * time.Second) // t0 + 30m
	hb()
	if reaped, _ := e.s.Reap(ctx); len(reaped) != 1 || e.ec2.State(inst) != "terminated" {
		t.Fatalf("not reaped 30 min after first going idle: %v", reaped)
	}
}

func TestOfflineRunnerReapedAfterTimeout(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_, inst, resp := e.launchAndEnroll(t)
	e.s.Heartbeat(ctx, natsd.RunnerHeartbeatSubject(resp.RunnerID), []byte(`{}`))
	e.clk.Advance(61 * time.Second)
	e.s.SweepOffline(ctx) // offline_since = t0+61s, and it never comes back
	e.clk.Advance(29 * time.Minute)
	if reaped, _ := e.s.Reap(ctx); len(reaped) != 0 {
		t.Fatal("offline runner reaped before the timeout")
	}
	e.clk.Advance(time.Minute)
	if reaped, _ := e.s.Reap(ctx); len(reaped) != 1 || e.ec2.State(inst) != "terminated" {
		t.Fatalf("offline runner not reaped after 30 min: %v", reaped)
	}
}

func TestNeverEnrolledIsReaped(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r, err := e.s.Launch(ctx, "go-claude")
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(14 * time.Minute)
	if reaped, _ := e.s.Reap(ctx); len(reaped) != 0 {
		t.Fatal("reaped before the enrol deadline")
	}
	e.clk.Advance(2 * time.Minute)
	if reaped, _ := e.s.Reap(ctx); len(reaped) != 1 || e.ec2.State(r.InstanceID.String) != "terminated" {
		t.Fatalf("stuck launch not reaped: %v", reaped)
	}
}

func TestEnrolRejections(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r, _ := e.s.Launch(ctx, "go-claude")
	tok := tokenFromUserData(t, e.ec2)
	good := proto.EnrollRequest{Token: tok, NKeyPublic: userKey(t), InstanceID: r.InstanceID.String, Facts: bigFacts}

	cases := map[string]struct {
		mut func(*proto.EnrollRequest)
		ip  string
	}{
		"wrong IP":       {func(*proto.EnrollRequest) {}, "10.0.9.9"},
		"wrong instance": {func(q *proto.EnrollRequest) { q.InstanceID = "i-0000" }, r.PrivateIp},
		"too small":      {func(q *proto.EnrollRequest) { q.Facts.VCPU = 2 }, r.PrivateIp},
		"wrong arch":     {func(q *proto.EnrollRequest) { q.Facts.Arch = "x86_64" }, r.PrivateIp},
		"unknown token":  {func(q *proto.EnrollRequest) { q.Token = "wre_nope" }, r.PrivateIp},
	}
	for name, c := range cases {
		q := good
		c.mut(&q)
		if _, err := e.s.Enroll(ctx, q, c.ip); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: want forbidden, got %v", name, err)
		}
	}
	bad := good
	bad.NKeyPublic = "SUAFAKE"
	if _, err := e.s.Enroll(ctx, bad, r.PrivateIp); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad nkey: %v", err)
	}
	// None of the failures consumed the token; the good request still works once.
	if _, err := e.s.Enroll(ctx, good, r.PrivateIp); err != nil {
		t.Fatalf("good enrol after rejections: %v", err)
	}
	if _, err := e.s.Enroll(ctx, good, r.PrivateIp); !errors.Is(err, ErrForbidden) {
		t.Fatalf("token reuse: %v", err)
	}
}

func TestAdoptedRunnerTokenFlow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tok, _, err := e.s.MintToken(ctx, "go-claude", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(11 * time.Minute)
	if _, err := e.s.Enroll(ctx, proto.EnrollRequest{Token: tok, NKeyPublic: userKey(t), Facts: bigFacts}, "192.168.1.20"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expired token: %v", err)
	}
	tok, _, _ = e.s.MintToken(ctx, "go-claude", 10*time.Minute)
	resp, err := e.s.Enroll(ctx, proto.EnrollRequest{Token: tok, NKeyPublic: userKey(t), Facts: bigFacts}, "192.168.1.20")
	if err != nil {
		t.Fatal(err)
	}
	e.s.Heartbeat(ctx, natsd.RunnerHeartbeatSubject(resp.RunnerID), []byte(`{}`))
	e.clk.Advance(5 * time.Hour)
	if reaped, _ := e.s.Reap(ctx); len(reaped) != 0 {
		t.Fatal("adopted runner reaped")
	}
	if err := e.s.Terminate(ctx, resp.Name); !errors.Is(err, ErrForbidden) {
		t.Fatalf("terminate adopted: %v", err)
	}
}

func TestLaunchRefusals(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.s.EnrollURL = "http://127.0.0.1:4223"
	if _, err := e.s.Launch(ctx, "go-claude"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("loopback enrol URL: %v", err)
	}
	e.s.EnrollURL = "http://10.0.1.5:4223"
	e.s.Prov = nil
	if _, err := e.s.Launch(ctx, "go-claude"); !errors.Is(err, ErrNoAWS) {
		t.Fatalf("no AWS: %v", err)
	}
	if len(e.ec2.Runs) != 0 {
		t.Fatal("launched despite refusal")
	}
}

func TestRunnerIDFromSubject(t *testing.T) {
	if id, ok := RunnerIDFromSubject("wr.r.42.hb"); !ok || id != 42 {
		t.Fatal(id, ok)
	}
	for _, s := range []string{"wr.r.x.hb", "wr.r.42.cmd", "wr.r.0.hb", "wr.r.-1.hb", "foo"} {
		if _, ok := RunnerIDFromSubject(s); ok {
			t.Errorf("accepted %q", s)
		}
	}
}

// A runner that blips offline while sessions are still running comes back busy, so it is not
// reaped, upgraded or drained until every session ends.
func TestOfflineReturnWithSessionsStaysBusy(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	name, _, resp := e.launchAndEnroll(t)
	hb := func() { e.s.Heartbeat(ctx, natsd.RunnerHeartbeatSubject(resp.RunnerID), []byte(`{}`)) }
	hb()
	r, _ := e.s.DB.RQ.GetRunnerByName(ctx, name)
	w := e.s.DB.W
	for _, q := range []string{
		`INSERT INTO project (slug, name) VALUES ('p','P')`,
		`INSERT INTO task (project_id, rank, title, sqid) VALUES (1,'V001','t','abc123')`,
	} {
		if _, err := w.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Exec(`INSERT INTO run (task_id, kind, attempt, state, runner_id) VALUES (1,'work',1,'running',?)`, r.ID); err != nil {
		t.Fatal(err)
	}
	w.Exec(`UPDATE runner SET state='busy', idle_since=NULL WHERE id=?`, r.ID)
	e.clk.Advance(61 * time.Second)
	e.s.SweepOffline(ctx)
	hb()
	if r, _ := e.s.DB.RQ.GetRunnerByName(ctx, name); r.State != "busy" || r.IdleSince.Valid {
		t.Fatalf("want busy with no idle clock after return, got %s idle_since=%v", r.State, r.IdleSince)
	}
}

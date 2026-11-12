package sessions

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/dbtime"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/sessionhost"
)

// fakeHost is a session host in memory.
type fakeHost struct {
	sess    map[string]sessionhost.Info
	starts  []sessionhost.Request
	stops   []string
	downErr error
	failErr error
	nextPID int
	now     func() time.Time
}

func newFake(now func() time.Time) *fakeHost {
	return &fakeHost{sess: map[string]sessionhost.Info{}, nextPID: 100, now: now}
}

func (f *fakeHost) List(context.Context) ([]sessionhost.Info, error) {
	if f.downErr != nil {
		return nil, f.downErr
	}
	var out []sessionhost.Info
	for _, i := range f.sess {
		out = append(out, i)
	}
	return out, nil
}

func (f *fakeHost) Start(_ context.Context, req sessionhost.Request) (sessionhost.Info, error) {
	f.starts = append(f.starts, req)
	if f.failErr != nil {
		return sessionhost.Info{}, f.failErr
	}
	f.nextPID++
	i := sessionhost.Info{Slug: req.Slug, Name: req.Name, Dir: req.Dir, PID: f.nextPID, State: "running", Started: f.now(), Resumed: req.Resume}
	f.sess[req.Slug] = i
	return i, nil
}

func (f *fakeHost) Stop(_ context.Context, slug string) error {
	f.stops = append(f.stops, slug)
	delete(f.sess, slug)
	return nil
}

func (f *fakeHost) exit(slug string, code int) {
	i := f.sess[slug]
	i.State, i.ExitCode = "exited", code
	f.sess[slug] = i
}

type env struct {
	r   *Reconciler
	h   *fakeHost
	d   *db.DB
	now time.Time
}

func (e *env) tick(d time.Duration) { e.now = e.now.Add(d) }

func newEnv(t *testing.T) *env {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	e := &env{d: d, now: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	e.h = newFake(func() time.Time { return e.now })
	e.r = &Reconciler{DB: d, Host: e.h, Dir: "/home/wrangl-agent/work/primary", Now: func() time.Time { return e.now }}
	exec := func(q string, a ...any) {
		if _, err := d.W.Exec(q, a...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO project (slug, name) VALUES ('alpha','A'), ('beta','B'), ('norepo','N')`)
	exec(`INSERT INTO repo (project_id, owner, name, default_branch, clone_url) VALUES (1,'acme','alpha','trunk','https://example.test/acme/alpha.git'), (2,'acme','beta','main','')`)
	return e
}

func (e *env) state(t *testing.T, slug string) string {
	t.Helper()
	var s string
	if err := e.d.R.QueryRow(`SELECT state FROM primary_session s JOIN project p ON p.id = s.project_id WHERE p.slug = ?`, slug).Scan(&s); err != nil {
		return "(none)"
	}
	return s
}

func (e *env) reconcile(t *testing.T) {
	t.Helper()
	if err := e.r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStartsOneSessionPerProjectWithARepo(t *testing.T) {
	e := newEnv(t)
	e.reconcile(t)
	if len(e.h.starts) != 2 {
		t.Fatalf("starts: %+v", e.h.starts)
	}
	a := e.h.starts[0]
	if a.Slug != "alpha" || a.Name != "alpha" || a.Dir != "/home/wrangl-agent/work/primary/alpha" || a.CloneURL != "https://example.test/acme/alpha.git" || a.Branch != "trunk" || a.Resume {
		t.Errorf("alpha request: %+v", a)
	}
	if b := e.h.starts[1]; b.CloneURL != "https://github.com/acme/beta.git" || b.Branch != "main" {
		t.Errorf("beta request: %+v", b)
	}
	if e.state(t, "alpha") != "running" || e.state(t, "norepo") != "(none)" {
		t.Errorf("states: %s %s", e.state(t, "alpha"), e.state(t, "norepo"))
	}
	// A second pass with everything up does nothing.
	e.reconcile(t)
	if len(e.h.starts) != 2 || len(e.h.stops) != 0 {
		t.Errorf("not idempotent: %d starts, %v stops", len(e.h.starts), e.h.stops)
	}
}

func TestTokenPassedToStartOnly(t *testing.T) {
	e := newEnv(t)
	e.r.Token = func(context.Context, dbgen.Repo) (string, error) { return "tok123", nil }
	e.reconcile(t)
	if e.h.starts[0].Token != "tok123" {
		t.Errorf("token not passed: %+v", e.h.starts[0])
	}
	if h := e.r.Health(context.Background(), "alpha"); strings.Contains(h, "tok123") {
		t.Error("token leaked into health")
	}
}

func TestRestartWithResumeAndBackoff(t *testing.T) {
	e := newEnv(t)
	e.reconcile(t)
	e.h.exit("alpha", 1)
	e.tick(10 * time.Second)
	e.reconcile(t) // first sight of the exit: backoff, no start yet
	if e.state(t, "alpha") != "backoff" || len(e.h.starts) != 2 {
		t.Fatalf("state %s, starts %d", e.state(t, "alpha"), len(e.h.starts))
	}
	if h := e.r.Health(context.Background(), "alpha"); !strings.Contains(h, "exited with code 1") || !strings.Contains(h, "restarting in 5s") {
		t.Errorf("health %q", h)
	}
	e.tick(2 * time.Second)
	e.reconcile(t)
	if len(e.h.starts) != 2 {
		t.Fatal("restarted before the backoff elapsed")
	}
	e.tick(4 * time.Second)
	e.reconcile(t)
	if len(e.h.starts) != 3 || !e.h.starts[2].Resume || e.state(t, "alpha") != "running" {
		t.Fatalf("restart: %+v state %s", e.h.starts, e.state(t, "alpha"))
	}
	// It dies again straight away: the backoff doubles.
	e.h.exit("alpha", 2)
	e.tick(time.Second)
	e.reconcile(t)
	e.tick(6 * time.Second)
	e.reconcile(t)
	if len(e.h.starts) != 3 {
		t.Fatal("second backoff should be 10s")
	}
	e.tick(5 * time.Second)
	e.reconcile(t)
	if len(e.h.starts) != 4 {
		t.Fatalf("starts %d", len(e.h.starts))
	}
	if h := e.r.Health(context.Background(), "alpha"); !strings.Contains(h, "restarted 2 times") {
		t.Errorf("health %q", h)
	}
	// Staying up long enough forgets the failures.
	e.tick(StableAfter + time.Second)
	e.reconcile(t)
	var f int64
	e.d.R.QueryRow(`SELECT failures FROM primary_session WHERE project_id = 1`).Scan(&f)
	if f != 0 {
		t.Errorf("failures = %d after a stable run", f)
	}
	if backoff(1) != MinBackoff || backoff(3) != 20*time.Second || backoff(50) != MaxBackoff {
		t.Errorf("backoff curve: %v %v %v", backoff(1), backoff(3), backoff(50))
	}
}

func TestHostLostItsSessionsResumes(t *testing.T) {
	e := newEnv(t)
	e.reconcile(t)
	e.h.sess = map[string]sessionhost.Info{} // the host restarted and forgot everything
	e.reconcile(t)
	if len(e.h.starts) != 4 || !e.h.starts[2].Resume || !e.h.starts[3].Resume {
		t.Fatalf("want both sessions started again with resume: %+v", e.h.starts)
	}
}

func TestStopOnArchiveAndUnlink(t *testing.T) {
	e := newEnv(t)
	e.reconcile(t)
	e.d.W.Exec(`UPDATE project SET archived_at = ? WHERE slug = 'alpha'`, dbtime.Format(e.now))
	e.d.W.Exec(`DELETE FROM repo WHERE project_id = 2`)
	e.h.sess["stranger"] = sessionhost.Info{Slug: "stranger", State: "running"} // not a project: left alone
	e.reconcile(t)
	if strings.Join(e.h.stops, ",") != "alpha,beta" {
		t.Errorf("stops %v", e.h.stops)
	}
	if _, ok := e.h.sess["stranger"]; !ok {
		t.Error("a session that is no project's was stopped")
	}
	if e.state(t, "alpha") != "stopped" || e.state(t, "beta") != "stopped" {
		t.Errorf("states %s %s", e.state(t, "alpha"), e.state(t, "beta"))
	}
	if e.r.Health(context.Background(), "alpha") != "stopped" {
		t.Error("health")
	}
	// Restoring starts it again, continuing the conversation.
	e.d.W.Exec(`UPDATE project SET archived_at = NULL WHERE slug = 'alpha'`)
	e.reconcile(t)
	if last := e.h.starts[len(e.h.starts)-1]; last.Slug != "alpha" || !last.Resume {
		t.Errorf("restore: %+v", last)
	}
}

func TestStartFailureBacksOffAndHostDown(t *testing.T) {
	e := newEnv(t)
	e.h.failErr = errors.New("clone failed: no such repo\nmore output")
	e.reconcile(t)
	if e.state(t, "alpha") != "failed" {
		t.Fatal(e.state(t, "alpha"))
	}
	if h := e.r.Health(context.Background(), "alpha"); !strings.Contains(h, "could not start: clone failed: no such repo") || strings.Contains(h, "more output") {
		t.Errorf("health %q", h)
	}
	n := len(e.h.starts)
	e.tick(time.Second)
	e.reconcile(t)
	if len(e.h.starts) != n {
		t.Error("retried inside the backoff")
	}
	e.h.failErr = nil
	e.tick(10 * time.Second)
	e.reconcile(t)
	if e.state(t, "alpha") != "running" || e.h.starts[len(e.h.starts)-1].Resume {
		t.Errorf("recovery: %s %+v", e.state(t, "alpha"), e.h.starts[len(e.h.starts)-1])
	}

	e.h.downErr = errors.New("connection refused")
	if err := e.r.Reconcile(context.Background()); err == nil {
		t.Error("an unreachable host should be reported")
	}
	if e.state(t, "alpha") != "host_down" {
		t.Errorf("state %s", e.state(t, "alpha"))
	}
	if h := e.r.Health(context.Background(), "alpha"); !strings.Contains(h, "session host unreachable") {
		t.Errorf("health %q", h)
	}
	e.h.downErr = nil
	e.reconcile(t)
	if e.state(t, "alpha") != "running" {
		t.Errorf("after the host came back: %s", e.state(t, "alpha"))
	}
}

func TestHealthEmptyWhenNothingRecorded(t *testing.T) {
	e := newEnv(t)
	if h := e.r.Health(context.Background(), "alpha"); h != "" {
		t.Errorf("%q", h)
	}
	if h := e.r.Health(context.Background(), "nope"); h != "" {
		t.Errorf("%q", h)
	}
}

// The same reconciler against the real session host over a real socket, with `sleep` as the child.
func TestAgainstRealSessionHost(t *testing.T) {
	e := newEnv(t)
	h := sessionhost.NewHost(sessionhost.Config{
		Argv:      func(sp sessionhost.Spec, resume bool) []string { return []string{"/bin/sleep", "30"} },
		StopGrace: time.Second,
	})
	sock := filepath.Join(t.TempDir(), "s.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		(&sessionhost.Server{Host: h, Path: sock, Group: "no-such-group", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Serve(ctx)
		close(done)
	}()
	t.Cleanup(func() { h.StopAll(); cancel(); <-done })
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	src := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "trunk"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "one"}} {
		c := osexec.Command("git", args...)
		c.Dir = src
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	e.d.W.Exec(`UPDATE repo SET clone_url = ?, default_branch = 'trunk'`, src)
	work := filepath.Join(t.TempDir(), "primary")
	e.r.Host = SocketHost{C: sessionhost.Client{Path: sock}}
	e.r.Dir = work
	e.r.Now = time.Now
	if err := e.r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.state(t, "alpha") != "running" || e.state(t, "beta") != "running" {
		t.Fatalf("states %s %s", e.state(t, "alpha"), e.state(t, "beta"))
	}
	if _, err := os.Stat(filepath.Join(work, "alpha", ".git")); err != nil {
		t.Errorf("no checkout made by the host: %v", err)
	}
	// Kill alpha's process: the next passes see the exit, wait out the backoff, resume with -c.
	var pid int64
	e.d.R.QueryRow(`SELECT pid FROM primary_session WHERE project_id = 1`).Scan(&pid)
	syscall.Kill(int(pid), syscall.SIGKILL)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.r.Reconcile(context.Background())
		if e.state(t, "alpha") == "backoff" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if e.state(t, "alpha") != "backoff" {
		t.Fatalf("exit not noticed: %s", e.state(t, "alpha"))
	}
	e.d.W.Exec(`UPDATE primary_session SET next_attempt_at = '2000-01-01T00:00:00.000Z'`)
	if err := e.r.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	var restarts int64
	e.d.R.QueryRow(`SELECT restarts FROM primary_session WHERE project_id = 1`).Scan(&restarts)
	if e.state(t, "alpha") != "running" || restarts != 1 {
		t.Fatalf("not resumed: %s restarts %d", e.state(t, "alpha"), restarts)
	}
	for _, i := range h.List() {
		if i.Slug == "alpha" && !i.Resumed {
			t.Error("alpha was not started with -c")
		}
	}
}

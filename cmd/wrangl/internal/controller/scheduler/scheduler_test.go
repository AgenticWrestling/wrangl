package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/fleet"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/provision"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type failingEC2 struct {
	*provision.Fake
	fail bool
}

func (f *failingEC2) RunInstances(ctx context.Context, in *ec2RunInput, o ...func(*ec2Options)) (*ec2RunOutput, error) {
	if f.fail {
		return nil, errInsufficientCapacity
	}
	return f.Fake.RunInstances(ctx, in, o...)
}

func setup(t *testing.T, maxRunners int) (*Scheduler, *failingEC2, *clock, *db.DB) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(dir, "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	keys, _ := natsd.EnsureKeys(filepath.Join(dir, "nats"))
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "wrangl-linux-aarch64"), []byte("bin"), 0o755)
	fake := &failingEC2{Fake: provision.NewFake()}
	clk := &clock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := &fleet.Service{DB: d, Keys: keys, Prov: &provision.Provisioner{EC2: fake, SSM: fake.Fake, ControllerID: "c"},
		ControllerID: "c", EnrollURL: "http://10.0.1.5:4223", BinDir: bin, Now: clk.Now, Log: log}
	yaml := []byte("name: go\narch: arm64\nmin: {vcpu: 2, ram: 4GiB, disk: 20GiB}\nmax_runners: " + itoa(maxRunners) + "\n")
	if _, err := f.CreateTemplate(context.Background(), yaml); err != nil {
		t.Fatal(err)
	}
	d.W.Exec(`INSERT INTO project (slug, name, default_template_id) VALUES ('p', 'P', 1)`)
	d.W.Exec(`INSERT INTO project_template_limit (project_id, template_id, max_instances) VALUES (1, 1, 100)`) // caps have their own tests
	return &Scheduler{Fleet: f, Now: clk.Now, Log: log}, fake, clk, d
}

func itoa(n int) string { return string(rune('0' + n)) }

func queue(t *testing.T, d *db.DB, n int) {
	for i := 0; i < n; i++ {
		var count int
		d.W.QueryRow(`SELECT COUNT(*) FROM task`).Scan(&count)
		res, err := d.W.Exec(`INSERT INTO task (project_id, rank, title) VALUES (1, ?, 't')`, fmt.Sprintf("V%03d", count+1))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		d.W.Exec(`INSERT INTO run (task_id) VALUES (?)`, id)
	}
}

func TestScaleUpToDemandWithinMax(t *testing.T) {
	s, fake, _, d := setup(t, 3)
	d.W.Exec(`UPDATE runner_template SET max_sessions = 1`) // one run per runner: demand maps 1:1 to hosts
	ctx := context.Background()
	if got, _ := s.Tick(ctx); len(got) != 0 {
		t.Fatalf("launched with no demand: %v", got)
	}
	queue(t, d, 2)
	got, err := s.Tick(ctx)
	if err != nil || got["go"] != 2 || len(fake.Runs) != 2 {
		t.Fatalf("demand 2: %v %v runs=%d", got, err, len(fake.Runs))
	}
	// Launching runners count as supply: no double launch.
	if got, _ := s.Tick(ctx); len(got) != 0 {
		t.Fatalf("relaunched: %v", got)
	}
	queue(t, d, 5) // demand 7, supply 2, max 3 -> one more
	if got, _ := s.Tick(ctx); got["go"] != 1 || len(fake.Runs) != 3 {
		t.Fatalf("max_runners: %v runs=%d", got, len(fake.Runs))
	}
	if got, _ := s.Tick(ctx); len(got) != 0 {
		t.Fatalf("exceeded max: %v", got)
	}
}

func TestCordonedRunnersAreNotSupply(t *testing.T) {
	s, fake, _, d := setup(t, 2)
	ctx := context.Background()
	queue(t, d, 1)
	s.Tick(ctx)
	d.W.Exec(`UPDATE runner SET cordoned = 1`)
	if got, _ := s.Tick(ctx); got["go"] != 1 || len(fake.Runs) != 2 {
		t.Fatalf("cordoned runner counted as supply: %v", got)
	}
}

func TestFailedTemplateVersionBlocksLaunch(t *testing.T) {
	s, fake, _, d := setup(t, 2)
	ctx := context.Background()
	d.W.Exec(`UPDATE runner_template SET failed_version = version`)
	queue(t, d, 1)
	if got, _ := s.Tick(ctx); len(got) != 0 || len(fake.Runs) != 0 {
		t.Fatalf("launched a failed template: %v", got)
	}
}

func TestBackoffAfterLaunchFailure(t *testing.T) {
	s, fake, clk, d := setup(t, 2)
	ctx := context.Background()
	queue(t, d, 1)
	fake.fail = true
	if _, err := s.Tick(ctx); err == nil {
		t.Fatal("want launch error")
	}
	fake.fail = false
	if got, _ := s.Tick(ctx); len(got) != 0 {
		t.Fatal("retried inside the backoff window")
	}
	clk.Advance(61 * time.Second)
	if got, _ := s.Tick(ctx); got["go"] != 1 {
		t.Fatalf("not retried after backoff: %v", got)
	}
	// Doubling: two failures in a row wait 1m then 2m.
	d.W.Exec(`UPDATE runner SET state = 'terminated'`)
	fake.fail = true
	s.Tick(ctx)
	clk.Advance(61 * time.Second)
	s.Tick(ctx) // second failure -> 2m
	fake.fail = false
	clk.Advance(61 * time.Second)
	if got, _ := s.Tick(ctx); len(got) != 0 {
		t.Fatal("backoff did not double")
	}
	clk.Advance(61 * time.Second)
	if got, _ := s.Tick(ctx); got["go"] != 1 {
		t.Fatalf("not retried after doubled backoff: %v", got)
	}
}

// With several sessions per runner, demand is met in hosts of max_sessions slots: five queued
// runs on a 4-slot template need two runners, not five, and launching runners count as supply.
func TestScaleUpCountsSessionSlots(t *testing.T) {
	s, fake, _, d := setup(t, 5)
	d.W.Exec(`UPDATE runner_template SET max_sessions = 4`)
	ctx := context.Background()
	queue(t, d, 3)
	if got, err := s.Tick(ctx); err != nil || got["go"] != 1 || len(fake.Runs) != 1 {
		t.Fatalf("3 runs fit one 4-slot host: %v %v runs=%d", got, err, len(fake.Runs))
	}
	queue(t, d, 1) // demand 4, supply 4: still no new host
	if got, _ := s.Tick(ctx); len(got) != 0 {
		t.Fatalf("the fourth run fits the first host: %v", got)
	}
	queue(t, d, 1) // demand 5, supply 4: one more host
	if got, _ := s.Tick(ctx); got["go"] != 1 || len(fake.Runs) != 2 {
		t.Fatalf("the fifth run needs a second host: %v runs=%d", got, len(fake.Runs))
	}
}

// A run whose repository the GitHub App cannot reach waits visibly (status text) and never
// touches the runner: it is not claimed and released every tick (which would reset its idle clock).
func TestUnreachableRepoWaitsWithoutTouchingRunner(t *testing.T) {
	s, _, _, d := setup(t, 1)
	ctx := context.Background()
	d.W.Exec(`UPDATE runner_template SET agent_json = '{"start":["true"]}'`)
	d.W.Exec(`INSERT INTO runner (name, template_id, state, idle_since, template_version) VALUES ('r1', 1, 'idle', '2026-01-01T00:00:00.000Z', 1)`)
	d.W.Exec(`INSERT INTO repo (project_id, owner, name, default_branch, gh_installation_id, clone_url) VALUES (1, 'o', 'n', 'main', 5, 'file:///x')`)
	queue(t, d, 1)
	d.W.Exec(`UPDATE run SET template_id = 1, repo_id = 1`)
	s.GH = failingGH{}
	for i := 0; i < 3; i++ {
		if _, err := s.Assign(ctx); err != nil {
			t.Fatalf("a missing App is a wait, not an error: %v", err)
		}
	}
	var state, idleSince, status string
	d.R.QueryRow(`SELECT state, idle_since FROM runner WHERE name = 'r1'`).Scan(&state, &idleSince)
	d.R.QueryRow(`SELECT status_text FROM run`).Scan(&status)
	if state != "idle" || idleSince != "2026-01-01T00:00:00.000Z" {
		t.Fatalf("the runner was touched: %s %s", state, idleSince)
	}
	if !strings.Contains(status, "not installed") {
		t.Fatalf("the run should say why it waits: %q", status)
	}
}

// failingGH is a GitHub whose installation cannot reach any repository.
type failingGH struct{ gh.API }

func (failingGH) RepoToken(_ context.Context, r gh.Repo) (string, time.Time, error) {
	return "", time.Time{}, &gh.NotInstalledError{Owner: r.Owner, Name: r.Name}
}

func TestRepeatedEnrolFailuresPauseLaunching(t *testing.T) {
	s, fake, clk, d := setup(t, 5)
	ctx := context.Background()
	var asked []int64
	s.OnLaunchStuck = func(template string, failed int64) { asked = append(asked, failed) }
	queue(t, d, 1)
	for i := 0; i < MaxEnrolFailures; i++ {
		if got, err := s.Tick(ctx); err != nil || got["go"] != 1 {
			t.Fatalf("launch %d: %v %v", i, got, err)
		}
		clk.Advance(fleet.EnrollDeadline + time.Minute)
		if reaped, err := s.Fleet.Reap(ctx); err != nil || len(reaped) != 1 {
			t.Fatalf("reap %d: %v %v", i, reaped, err)
		}
	}
	if got, _ := s.Tick(ctx); len(got) != 0 || len(fake.Runs) != MaxEnrolFailures {
		t.Fatalf("launched after %d failed enrolments: %v runs=%d", MaxEnrolFailures, got, len(fake.Runs))
	}
	s.Tick(ctx)
	if len(asked) != 1 || asked[0] != MaxEnrolFailures {
		t.Fatalf("owner asked %v, want once with %d", asked, MaxEnrolFailures)
	}
	// The failures age out of the window: launching resumes.
	clk.Advance(EnrolFailWindow + time.Minute)
	if got, _ := s.Tick(ctx); got["go"] != 1 {
		t.Fatalf("did not resume after the window: %v", got)
	}
}

func TestEffectiveCap(t *testing.T) {
	null, v := sql.NullInt64{}, func(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
	for _, c := range []struct {
		stored sql.NullInt64
		def    bool
		max    int64
		want   int64
	}{
		{null, true, 5, 1},  // no row: default template gets 1
		{null, false, 5, 0}, // no row: other templates are opt-in
		{v(3), false, 5, 3},
		{v(9), false, 5, 5}, // the template ceiling wins
		{v(0), true, 5, 0},
		{v(2), true, 0, 0},
	} {
		if got := EffectiveCap(c.stored, c.def, c.max); got != c.want {
			t.Errorf("EffectiveCap(%v,%v,%d) = %d, want %d", c.stored, c.def, c.max, got, c.want)
		}
	}
}

func TestPlanLaunches(t *testing.T) {
	got := PlanLaunches(10, 4, 10, []ProjectDemand{
		{Project: 1, Demand: 6, Live: 0, Cap: 1}, // wants 2 hosts, capped at 1
		{Project: 2, Demand: 4, Live: 0, Cap: 5}, // 1 host; project 1's overflow does not spill here
		{Project: 3, Demand: 4, Live: 2, Cap: 2}, // at its cap
	})
	want := []Launch{{1, 1}, {2, 1}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := PlanLaunches(8, 4, 1, []ProjectDemand{{1, 4, 0, 5}, {2, 4, 0, 5}}); len(got) != 1 {
		t.Fatalf("total must respect max hosts: %v", got)
	}
}

// With the default caps a project launches one instance of its default template and none of any
// other; raising its cap lets the next tick launch more, attributed to the project.
func TestProjectCapLimitsLaunches(t *testing.T) {
	s, fake, _, d := setup(t, 5)
	d.W.Exec(`UPDATE runner_template SET max_sessions = 1`)
	d.W.Exec(`DELETE FROM project_template_limit`) // back to the default rule: 1 for the default template
	ctx := context.Background()
	queue(t, d, 3)
	if got, _ := s.Tick(ctx); got["go"] != 1 {
		t.Fatalf("want 1 launch under the default cap, got %v", got)
	}
	var forProject int64
	d.W.QueryRow(`SELECT launched_for_project FROM runner`).Scan(&forProject)
	if forProject != 1 {
		t.Fatalf("launched_for_project = %d", forProject)
	}
	if got, _ := s.Tick(ctx); len(got) != 0 {
		t.Fatalf("launched past the cap: %v", got)
	}
	d.W.Exec(`INSERT INTO project_template_limit VALUES (1, 1, 3)`)
	if got, _ := s.Tick(ctx); got["go"] != 2 {
		t.Fatalf("want 2 more after raising the cap, got %v", got)
	}
	d.W.Exec(`UPDATE project_template_limit SET max_instances = 0`)
	_ = fake
}

// LaunchFor (the controller agent's fleet.launch) respects the project's cap and names it when refusing.
func TestLaunchForRespectsCap(t *testing.T) {
	s, _, _, d := setup(t, 5)
	ctx := context.Background()
	d.W.Exec(`DELETE FROM project_template_limit`) // default rule: 1 for the default template
	if _, err := s.LaunchFor(ctx, "p", "go", 2); err == nil || !strings.Contains(err.Error(), "at most 1 instance(s) of template go") {
		t.Fatalf("over cap: %v", err)
	}
	names, err := s.LaunchFor(ctx, "p", "go", 1)
	if err != nil || len(names) != 1 {
		t.Fatalf("within cap: %v %v", names, err)
	}
	var forProject int64
	d.W.QueryRow(`SELECT launched_for_project FROM runner`).Scan(&forProject)
	if forProject != 1 {
		t.Fatalf("launched_for_project = %d", forProject)
	}
	if _, err := s.LaunchFor(ctx, "p", "go", 1); err == nil || !strings.Contains(err.Error(), "has 1") {
		t.Fatalf("second: %v", err)
	}
	d.W.Exec(`INSERT INTO project_template_limit VALUES (1, 1, 0)`)
	if _, err := s.LaunchFor(ctx, "p", "go", 1); err == nil || !strings.Contains(err.Error(), "project limit 0") {
		t.Fatalf("zero cap: %v", err)
	}
}

// Remote templates launch a target machine (no enrolling runner); the queued run also counts as
// demand on its driver template; the project's cap for the remote template governs the target.
func TestRemoteTemplateLaunchesTargetWithinCap(t *testing.T) {
	s, _, _, d := setup(t, 3)
	ctx := context.Background()
	d.W.Exec(`UPDATE runner_template SET max_sessions = 1`)
	mac := "name: mac\narch: arm64\nmin: {vcpu: 2, ram: 4GiB, disk: 20GiB}\nmax_runners: 2\nmax_sessions: 1\nmode: remote\nconnection: {driver_template: go, secret: k}\n"
	if _, err := s.Fleet.CreateTemplate(ctx, []byte(mac)); err != nil {
		t.Fatal(err)
	}
	// the project's tasks run on mac (template 2); caps start at the default rule: 0 for mac
	d.W.Exec(`INSERT INTO task (project_id, rank, title, template_id) VALUES (1, 'V001', 'ios', 2)`)
	d.W.Exec(`INSERT INTO run (task_id, template_id) VALUES (1, 2)`)
	d.W.Exec(`DELETE FROM project_template_limit`)
	if got, _ := s.Tick(ctx); len(got) != 0 {
		t.Fatalf("a project capped at 0 targets gets neither a target nor an idle driver: %v", got)
	}
	var targets int
	d.R.QueryRow(`SELECT COUNT(*) FROM remote_target`).Scan(&targets)
	if targets != 0 {
		t.Fatalf("launched %d targets over a cap of 0", targets)
	}
	d.W.Exec(`INSERT INTO project_template_limit VALUES (1, 2, 1)`)
	got, err := s.Tick(ctx)
	if err != nil || got["mac"] != 1 || got["go"] != 1 {
		t.Fatalf("want one target and one driver runner within the cap: %v %v", got, err)
	}
	var state string
	var forProject int64
	d.R.QueryRow(`SELECT state, launched_for_project FROM remote_target`).Scan(&state, &forProject)
	if state != "launching" || forProject != 1 {
		t.Fatalf("target: %s for project %d", state, forProject)
	}
	var runners int
	d.R.QueryRow(`SELECT COUNT(*) FROM runner WHERE template_id = 2`).Scan(&runners)
	if runners != 0 {
		t.Fatal("a remote template must not create runner rows")
	}
	// the launching target already supplies the session: no second target for the same run
	if got, _ := s.Tick(ctx); got["mac"] != 0 {
		t.Fatalf("second target launched for one run: %v", got)
	}
}

// fleet.launch on a remote Mac template starts a target (not an enrolling runner), respects the
// project's cap (0 by default) and carries the 24-hour-minimum cost warning back to the caller.
func TestLaunchForMacTargetWarnsAndRespectsCap(t *testing.T) {
	s, fake, _, d := setup(t, 3)
	fake.Types = append(fake.Types, ec2types.InstanceTypeInfo{InstanceType: "mac2.metal", VCpuInfo: &ec2types.VCpuInfo{DefaultVCpus: aws.Int32(8)},
		MemoryInfo: &ec2types.MemoryInfo{SizeInMiB: aws.Int64(16384)}, ProcessorInfo: &ec2types.ProcessorInfo{SupportedArchitectures: []ec2types.ArchitectureType{ec2types.ArchitectureTypeArm64}}})
	ctx := context.Background()
	mac := "name: mac\narch: arm64\ninstance_types: [mac2.metal]\nmin: {vcpu: 2, ram: 4GiB, disk: 20GiB}\nmax_runners: 1\nmode: remote\nconnection: {driver_template: go, secret: k}\n"
	if _, err := s.Fleet.CreateTemplate(ctx, []byte(mac)); err != nil {
		t.Fatal(err)
	}
	d.W.Exec(`DELETE FROM project_template_limit`)
	if _, err := s.LaunchFor(ctx, "p", "mac", 1); err == nil || !strings.Contains(err.Error(), "at most 0 instance(s) of template mac") {
		t.Fatalf("opt-in: want a refusal naming the cap of 0, got %v", err)
	}
	d.W.Exec(`INSERT INTO project_template_limit VALUES (1, 2, 1)`)
	names, err := s.LaunchFor(ctx, "p", "mac", 1)
	if err != nil || len(names) != 1 || !strings.Contains(names[0], "24-hour minimum") {
		t.Fatalf("want a launch carrying the cost warning: %v %v", names, err)
	}
	var targets, runners int
	d.R.QueryRow(`SELECT COUNT(*) FROM remote_target WHERE launched_for_project = 1`).Scan(&targets)
	d.R.QueryRow(`SELECT COUNT(*) FROM runner WHERE template_id = 2`).Scan(&runners)
	if targets != 1 || runners != 0 {
		t.Fatalf("targets %d runners %d", targets, runners)
	}
	var warned int
	d.R.QueryRow(`SELECT COUNT(*) FROM log_entry WHERE level = 'warn' AND title LIKE 'Cost warning%'`).Scan(&warned)
	if warned != 1 {
		t.Fatalf("a cost warning must be recorded on launch, got %d", warned)
	}
	if _, err := s.LaunchFor(ctx, "p", "mac", 1); err == nil || !strings.Contains(err.Error(), "has 1") {
		t.Fatalf("second Mac over the cap: %v", err)
	}
}

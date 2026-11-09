package scheduler

import (
	"context"
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

func TestVersionBehindRunnersAreNotSupply(t *testing.T) {
	s, fake, _, d := setup(t, 2)
	ctx := context.Background()
	d.W.Exec(`INSERT INTO runner (name, template_id, state, idle_since, template_version) VALUES ('old', 1, 'idle', '2026-01-01T00:00:00.000Z', 1)`)
	d.W.Exec(`UPDATE runner_template SET version = 2`)
	queue(t, d, 1)
	if got, _ := s.Tick(ctx); got["go"] != 1 || len(fake.Runs) != 1 {
		t.Fatalf("a runner behind on the template version was counted as supply: %v", got)
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

// Launches are attributed to the project whose queued runs asked for them.
func TestLaunchesAreAttributedToTheProject(t *testing.T) {
	s, _, _, d := setup(t, 5)
	d.W.Exec(`UPDATE runner_template SET max_sessions = 1`)
	ctx := context.Background()
	queue(t, d, 3)
	if got, _ := s.Tick(ctx); got["go"] != 3 {
		t.Fatalf("want 3 launches (no per-project cap any more), got %v", got)
	}
	var forProject, n int64
	d.W.QueryRow(`SELECT launched_for_project, COUNT(*) FROM runner`).Scan(&forProject, &n)
	if forProject != 1 || n != 3 {
		t.Fatalf("launched_for_project = %d over %d runners", forProject, n)
	}
	if got, _ := s.Tick(ctx); len(got) != 0 {
		t.Fatalf("the launching runners already supply the sessions: %v", got)
	}
}

// LaunchFor (the controller agent's fleet.launch) respects the template's max_runners and names it when refusing.
func TestLaunchForRespectsMaxRunners(t *testing.T) {
	s, _, _, d := setup(t, 2)
	ctx := context.Background()
	if _, err := s.LaunchFor(ctx, "p", "go", 3); err == nil || !strings.Contains(err.Error(), "at most 2 instance(s)") {
		t.Fatalf("over max_runners: %v", err)
	}
	names, err := s.LaunchFor(ctx, "p", "go", 2)
	if err != nil || len(names) != 2 {
		t.Fatalf("within max_runners: %v %v", names, err)
	}
	var forProject int64
	d.W.QueryRow(`SELECT launched_for_project FROM runner LIMIT 1`).Scan(&forProject)
	if forProject != 1 {
		t.Fatalf("launched_for_project = %d", forProject)
	}
	if _, err := s.LaunchFor(ctx, "p", "go", 1); err == nil || !strings.Contains(err.Error(), "has 2") {
		t.Fatalf("third: %v", err)
	}
}

// Remote templates launch a target machine (no enrolling runner); the queued run also counts as
// demand on its driver template; the template's max_runners governs the targets.
func TestRemoteTemplateLaunchesTarget(t *testing.T) {
	s, _, _, d := setup(t, 3)
	ctx := context.Background()
	d.W.Exec(`UPDATE runner_template SET max_sessions = 1`)
	mac := "name: mac\narch: arm64\nmin: {vcpu: 2, ram: 4GiB, disk: 20GiB}\nmax_runners: 2\nmax_sessions: 1\nmode: remote\nconnection: {driver_template: go, secret: k}\n"
	if _, err := s.Fleet.CreateTemplate(ctx, []byte(mac)); err != nil {
		t.Fatal(err)
	}
	// the project's tasks run on mac (template 2)
	d.W.Exec(`INSERT INTO task (project_id, rank, title, template_id) VALUES (1, 'V001', 'ios', 2)`)
	d.W.Exec(`INSERT INTO run (task_id, template_id) VALUES (1, 2)`)
	got, err := s.Tick(ctx)
	if err != nil || got["mac"] != 1 || got["go"] != 1 {
		t.Fatalf("want one target and one driver runner: %v %v", got, err)
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
// template's max_runners and carries the 24-hour-minimum cost warning back to the caller.
func TestLaunchForMacTargetWarnsAndRespectsMaxRunners(t *testing.T) {
	s, fake, _, d := setup(t, 3)
	fake.Types = append(fake.Types, ec2types.InstanceTypeInfo{InstanceType: "mac2.metal", VCpuInfo: &ec2types.VCpuInfo{DefaultVCpus: aws.Int32(8)},
		MemoryInfo: &ec2types.MemoryInfo{SizeInMiB: aws.Int64(16384)}, ProcessorInfo: &ec2types.ProcessorInfo{SupportedArchitectures: []ec2types.ArchitectureType{ec2types.ArchitectureTypeArm64}}})
	ctx := context.Background()
	mac := "name: mac\narch: arm64\ninstance_types: [mac2.metal]\nmin: {vcpu: 2, ram: 4GiB, disk: 20GiB}\nmax_runners: 1\nmode: remote\nconnection: {driver_template: go, secret: k}\n"
	if _, err := s.Fleet.CreateTemplate(ctx, []byte(mac)); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("second Mac over max_runners: %v", err)
	}
}

func TestArchivedProjectIsInert(t *testing.T) {
	s, fake, _, d := setup(t, 3)
	ctx := context.Background()
	queue(t, d, 2)
	d.W.Exec(`UPDATE project SET archived_at = '2026-10-31T11:00:00.000Z' WHERE id = 1`)
	if got, err := s.Tick(ctx); err != nil || len(got) != 0 || len(fake.Runs) != 0 {
		t.Fatalf("launched for an archived project: %v %v runs=%d", got, err, len(fake.Runs))
	}
	if qs, _ := d.RQ.QueuedRunsForAssignment(ctx); len(qs) != 0 {
		t.Fatalf("archived project's runs offered for assignment: %d", len(qs))
	}
	if _, err := s.LaunchFor(ctx, "p", "go", 1); err == nil {
		t.Fatal("LaunchFor accepted an archived project")
	}
	d.W.Exec(`UPDATE project SET archived_at = NULL WHERE id = 1`)
	if got, _ := s.Tick(ctx); got["go"] == 0 {
		t.Fatalf("restored project not served: %v", got)
	}
}

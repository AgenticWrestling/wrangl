package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

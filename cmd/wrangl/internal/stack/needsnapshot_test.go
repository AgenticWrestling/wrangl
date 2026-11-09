package stack_test

import (
	"context"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/placement"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

// ADR 0009 section 5: the resolved need is written to the run when it is queued and never changes
// afterwards; a retry (a new run) sees the new values.
func TestQueueSnapshotsResolvedNeed(t *testing.T) {
	s, d := setup(t)
	ctx := context.Background()
	pl := &placement.Service{DB: d}
	get := func(run int64) (v, r, k int64, src string) {
		d.R.QueryRow(`SELECT need_vcpu, need_ram_mib, need_disk_gib, need_source FROM run WHERE id = ?`, run).Scan(&v, &r, &k, &src)
		return
	}

	a, err := s.Create(ctx, stack.CreateParams{Project: "p", Title: "a", Repo: "o/a", Actor: stack.User})
	if err != nil {
		t.Fatal(err)
	}
	// Defaults 0/0/0: legacy behaviour.
	run1, err := s.Queue(ctx, a.Sqid.String)
	if err != nil {
		t.Fatal(err)
	}
	if v, r, k, _ := get(run1.ID); v != 0 || r != 0 || k != 0 {
		t.Fatalf("default snapshot %d/%d/%d", v, r, k)
	}
	d.W.Exec(`UPDATE run SET state = 'cancelled'`)

	pl.SetGlobal(ctx, map[string]string{"vcpu": "1", "ram": "2GiB", "disk": "5GiB"})
	pl.SetProject(ctx, 1, map[string]string{"ram": "4GiB"})
	if err := s.SetNeed(ctx, a.Sqid.String, map[string]string{"disk": "10GiB"}, stack.User); err != nil {
		t.Fatal(err)
	}
	run2, err := s.Queue(ctx, a.Sqid.String)
	if err != nil {
		t.Fatal(err)
	}
	v, r, k, src := get(run2.ID)
	if v != 1 || r != 4096 || k != 10 || src != `{"disk":"task","ram":"project","vcpu":"default"}` {
		t.Fatalf("snapshot %d/%d/%d %s", v, r, k, src)
	}

	// Edits after queueing do not touch the queued run; the next run picks them up.
	pl.SetGlobal(ctx, map[string]string{"vcpu": "2"})
	s.SetNeed(ctx, a.Sqid.String, map[string]string{"ram": "8GiB"}, stack.User)
	if v, r, _, _ := get(run2.ID); v != 1 || r != 4096 {
		t.Fatalf("queued run changed: %d/%d", v, r)
	}
	d.W.Exec(`UPDATE run SET state = 'cancelled'`)
	run3, _ := s.Queue(ctx, a.Sqid.String)
	if v, r, _, src := get(run3.ID); v != 2 || r != 8192 || src != `{"disk":"task","ram":"task","vcpu":"default"}` {
		t.Fatalf("new run: %d/%d %s", v, r, src)
	}
}

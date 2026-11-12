package placement

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

func TestParseValueAndFloors(t *testing.T) {
	for _, c := range []struct {
		f, in string
		want  int64
		bad   bool
	}{
		{"vcpu", "2", 2, false}, {"vcpu", "0", 0, true}, {"vcpu", "2GiB", 0, true},
		{"ram", "2GiB", 2048, false}, {"ram", "512MiB", 512, false}, {"ram", "1024", 1024, false}, {"ram", "256MiB", 0, true}, {"ram", "x", 0, true},
		{"disk", "5GiB", 5, false}, {"disk", "500MiB", 1, false}, {"disk", "1500MiB", 2, false}, {"disk", "0", 0, true},
		{"gpu", "1", 0, true},
	} {
		got, err := ParseValue(c.f, c.in)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("ParseValue(%s,%q) = %d, %v; want %d bad=%v", c.f, c.in, got, err, c.want, c.bad)
		}
	}
}

func setup(t *testing.T) (*Service, *db.DB) {
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	d.W.Exec(`INSERT INTO project (slug, name) VALUES ('p', 'P')`)
	d.W.Exec(`INSERT INTO task (project_id, rank, title) VALUES (1, 'V001', 't')`)
	return &Service{DB: d}, d
}

func TestResolveLayersAndRaiseOnly(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	if g, _ := s.Global(ctx); g.VCPU != 0 || g.RAMMiB != 0 || g.DiskGiB != 0 {
		t.Fatalf("defaults must be 0/0/0: %+v", g)
	}
	if _, err := s.SetGlobal(ctx, map[string]string{"vcpu": "1", "ram": "2GiB", "disk": "5GiB"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetGlobal(ctx, map[string]string{"ram": ""}); err == nil {
		t.Fatal("clearing a default must fail")
	}
	if _, err := s.SetProject(ctx, 1, map[string]string{"ram": "4GiB"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTask(ctx, 1, 1, map[string]string{"disk": "10GiB"}, false); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Resolve(ctx, 1, 1)
	if r.VCPU != 1 || r.RAMMiB != 4096 || r.DiskGiB != 10 || r.Source["vcpu"] != "default" || r.Source["ram"] != "project" || r.Source["disk"] != "task" {
		t.Fatalf("%+v", r)
	}
	// The owner may lower below the project value and clear back to inherit.
	if _, err := s.SetTask(ctx, 1, 1, map[string]string{"ram": "1GiB"}, false); err != nil {
		t.Fatal(err)
	}
	if r, _ = s.Resolve(ctx, 1, 1); r.RAMMiB != 1024 || r.Source["ram"] != "task" {
		t.Fatalf("%+v", r)
	}
	if _, err := s.SetTask(ctx, 1, 1, map[string]string{"ram": ""}, false); err != nil {
		t.Fatal(err)
	}
	if r, _ = s.Resolve(ctx, 1, 1); r.RAMMiB != 4096 {
		t.Fatalf("clear: %+v", r)
	}
	// An agent may only raise (never below the effective value, never clear).
	for _, p := range []map[string]string{{"disk": "5GiB"}, {"disk": ""}, {"ram": "2GiB"}} {
		if _, err := s.SetTask(ctx, 1, 1, p, true); !errors.Is(err, errs.Forbidden) {
			t.Errorf("agent patch %v: %v", p, err)
		}
	}
	if _, err := s.SetTask(ctx, 1, 1, map[string]string{"disk": "20GiB", "ram": "8GiB"}, true); err != nil {
		t.Fatal(err)
	}
	if r, _ = s.Resolve(ctx, 1, 1); r.DiskGiB != 20 || r.RAMMiB != 8192 {
		t.Fatalf("%+v", r)
	}
}

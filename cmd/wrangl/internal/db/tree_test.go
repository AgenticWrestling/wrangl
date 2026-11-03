package db_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/sqids/sqids-go"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbtypes"
)

func TestTaskTree(t *testing.T) {
	d, _ := open(t)
	db := d.W
	ctx := context.Background()
	q := d.Q
	ex := func(s string, a ...any) error { _, err := db.ExecContext(ctx, s, a...); return err }
	ex(`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('go','','{}',1,1,1)`)
	ex(`INSERT INTO project (slug, name) VALUES ('a','A'), ('b','B')`)
	ex(`INSERT INTO repo (project_id, owner, name) VALUES (1,'o','r')`)
	sq, _ := sqids.New(sqids.Options{MinLength: 6})
	mk := func(parent sql.NullInt64, rank, title string) int64 {
		id, err := q.CreateTask(ctx, dbgen.CreateTaskParams{ProjectID: 1, ParentID: parent, Rank: rank, Title: title, CreatedBy: "user", TaskType: dbtypes.TaskCode})
		if err != nil {
			t.Fatal(title, err)
		}
		s, _ := sq.Encode([]uint64{uint64(id)})
		if err := q.SetTaskSqid(ctx, dbgen.SetTaskSqidParams{Sqid: sql.NullString{String: s, Valid: true}, ID: id}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	p := func(id int64) sql.NullInt64 { return sql.NullInt64{Int64: id, Valid: true} }
	root := mk(sql.NullInt64{}, "V", "epic")
	c1 := mk(p(root), "V", "c1") // container
	c1a := mk(p(c1), "V", "c1a")
	c1b := mk(p(c1), "W", "c1b")
	c2 := mk(p(root), "VV", "c2") // between V and W lexically
	c3 := mk(p(root), "W", "c3")
	_ = c1b

	rows, err := q.SubtreeOrdered(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, r := range rows {
		order = append(order, r.Title)
	}
	if got := strings.Join(order, ","); got != "epic,c1,c1a,c1b,c2,c3" {
		t.Fatalf("order %s", got)
	}

	// c1a depends on c3 (other subtree) -> next ready should skip to c1b
	if err := q.AddPrecursor(ctx, dbgen.AddPrecursorParams{TaskID: c1a, DependsOnID: c3, CreatedBy: "user"}); err != nil {
		t.Fatal(err)
	}
	const runA = 7
	n, err := q.NextReadyLeaf(ctx, dbgen.NextReadyLeafParams{RootID: root, RunID: sql.NullInt64{Int64: runA, Valid: true}})
	if err != nil || n.Title != "c1b" {
		t.Fatalf("next %v %v", n, err)
	}
	// cycle: would c3 -> c1a cycle? c1a reaches c3, so adding c3 depends-on c1a cycles
	hit, _ := q.PrecursorReaches(ctx, dbgen.PrecursorReachesParams{FromID: c1a, ToID: c3})
	if hit != 1 {
		t.Fatal("cycle not detected")
	}
	anc, _ := q.IsAncestorOrSelf(ctx, dbgen.IsAncestorOrSelfParams{NodeID: c1a, AncestorID: root})
	if anc != 1 {
		t.Fatal("ancestor not detected")
	}
	// complete c1b, c2, c3 in run A -> c1a ready for run A (same branch) ...
	for _, id := range []int64{c1b, c2, c3} {
		if err := q.CompleteTask(ctx, dbgen.CompleteTaskParams{ID: id, CompletedRunID: sql.NullInt64{Int64: runA, Valid: true}}); err != nil {
			t.Fatal(err)
		}
	}
	n, _ = q.NextReadyLeaf(ctx, dbgen.NextReadyLeafParams{RootID: root, RunID: sql.NullInt64{Int64: runA, Valid: true}})
	if n.Title != "c1a" {
		t.Fatalf("next after deps %v", n)
	}
	// ... but not for a different run (c3 not merged yet)
	if _, err := q.NextReadyLeaf(ctx, dbgen.NextReadyLeafParams{RootID: c1, RunID: sql.NullInt64{Int64: 99, Valid: true}}); err != sql.ErrNoRows {
		t.Fatalf("other run should be blocked: %v", err)
	}
	// merge -> done -> ready for anyone
	q.MarkRunTasksDone(ctx, sql.NullInt64{Int64: runA, Valid: true})
	if n, _ := q.NextReadyLeaf(ctx, dbgen.NextReadyLeafParams{RootID: c1, RunID: sql.NullInt64{Int64: 99, Valid: true}}); n.Title != "c1a" {
		t.Fatal("merged precursor should satisfy")
	}

	// lock: a run on c1 locks root (ancestor) and c1a (descendant), not c2's siblings outside
	ex(`INSERT INTO run (task_id, state) VALUES (?, 'running')`, c1)
	for _, id := range []int64{root, c1, c1a} {
		if l, _ := q.SubtreeLocked(ctx, id); l != 1 {
			t.Fatalf("expected lock on %d", id)
		}
	}
	if l, _ := q.SubtreeLocked(ctx, c2); l != 0 {
		t.Fatal("c2 should be free")
	}

	// sqid immutable, ranks unique among siblings (incl. NULL parent), parent same project
	if err := ex(`UPDATE task SET sqid = 'zzzzzz' WHERE id = ?`, root); err == nil {
		t.Fatal("sqid mutable")
	}
	if err := ex(`INSERT INTO task (project_id, parent_id, rank, title) VALUES (1, ?, 'V', 'dup')`, root); err == nil {
		t.Fatal("dup child rank")
	}
	if err := ex(`INSERT INTO task (project_id, rank, title) VALUES (1, 'V', 'duproot')`); err == nil {
		t.Fatal("dup root rank")
	}
	if err := ex(`INSERT INTO task (project_id, parent_id, rank, title) VALUES (2, ?, 'z', 'x')`, root); err == nil {
		t.Fatal("cross-project parent")
	}
	ex(`INSERT INTO task (project_id, rank, title) VALUES (2, 'V', 'other')`)
	if err := ex(`INSERT INTO task_dependency (task_id, depends_on_id) VALUES (?, last_insert_rowid())`, c2); err == nil {
		t.Fatal("cross-project precursor")
	}
	// cascade delete subtree
	ex(`DELETE FROM run`)
	if err := ex(`DELETE FROM task WHERE id = ?`, root); err != nil {
		t.Fatal(err)
	}
	var cnt int
	db.QueryRow(`SELECT count(*) FROM task WHERE project_id = 1`).Scan(&cnt)
	if cnt != 0 {
		t.Fatalf("cascade left %d", cnt)
	}
}

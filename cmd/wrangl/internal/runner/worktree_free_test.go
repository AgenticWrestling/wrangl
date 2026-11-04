package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// A review/revise/Smart Merge run on a branch that an earlier (blocked) run still has checked out
// must be able to take it over, keeping the old HEAD under refs/wrangl/stale.
func TestFreeBranchTakesOverStaleWorktree(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	mirror := filepath.Join(root, "m.git")
	git(t, root, "init", "-q", "--bare", "-b", "main", mirror)
	seed := filepath.Join(root, "seed")
	git(t, root, "clone", "-q", mirror, seed)
	os.WriteFile(filepath.Join(seed, "a"), []byte("1"), 0o644)
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-qm", "base")
	git(t, seed, "push", "-q", "origin", "HEAD:main")

	old := filepath.Join(root, "work", "40")
	git(t, mirror, "worktree", "add", "-B", "wrangl/x-2", old, "main")
	oldHead := git(t, old, "rev-parse", "HEAD")

	w := &Worker{runs: map[int64]*runState{}}
	s := &session{w: w, mirror: mirror, a: proto.Assignment{Branch: "wrangl/x-2"}}
	newDir := filepath.Join(root, "work", "52")

	// still live: refuse
	w.runs[40] = &runState{id: 40}
	if err := w.freeBranch(ctx, s, newDir); err == nil {
		t.Fatal("took a branch from a live run")
	}
	// finished: take over
	delete(w.runs, 40)
	if err := w.freeBranch(ctx, s, newDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Fatal("stale worktree still there")
	}
	if got := git(t, mirror, "rev-parse", "refs/wrangl/stale/40"); got != oldHead {
		t.Fatalf("stale ref %s, want %s", got, oldHead)
	}
	git(t, mirror, "worktree", "add", "-B", "wrangl/x-2", newDir, "main") // now succeeds
}

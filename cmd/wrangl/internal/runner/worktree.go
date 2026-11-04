package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Sessions on one runner share a bare clone ("mirror") per repo under <agent home>/work/.repos
// and each works in its own git worktree of it, so concurrent runs of the same repo share one
// object store (one clone, one fetch) but never touch each other's files, index or branch.
// Everything that writes shared metadata (fetch, worktree add/remove, branch delete) runs under
// the mirror's lock; the agent's own commits are ordinary git object writes, which git makes safe.

// mirrorPath is the shared bare clone for a clone URL.
func mirrorPath(home, cloneURL string) string {
	sum := sha256.Sum256([]byte(cloneURL))
	return filepath.Join(home, "work", ".repos", hex.EncodeToString(sum[:8])+".git")
}

func (w *Worker) lockRepo(mirror string) func() {
	v, _ := w.repoLocks.LoadOrStore(mirror, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// gitDir is the per-worktree git directory: dir/.git is a directory in a clone but a
// "gitdir: <path>" file in a linked worktree.
func gitDir(dir string) string {
	p := filepath.Join(dir, ".git")
	if fi, err := os.Stat(p); err != nil || fi.IsDir() {
		return p
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return p
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
}

// addWorktree creates s.dir as a worktree of the repo's shared mirror on a.Branch, starting from
// start (the base branch, or the existing run branch for a continuation).
func (w *Worker) addWorktree(ctx context.Context, s *session, start string) error {
	a, id, dir := s.a, s.id, s.dir
	s.mirror = mirrorPath(id.home, a.CloneURL)
	if err := os.MkdirAll(filepath.Dir(s.mirror), 0o755); err != nil {
		return err
	}
	unlock := w.lockRepo(s.mirror)
	defer unlock()
	if _, err := os.Stat(filepath.Join(s.mirror, "HEAD")); err != nil {
		os.RemoveAll(s.mirror)
		if out, err := gitAsDaemon(ctx, filepath.Dir(s.mirror), a.GitToken, "clone", "--bare", a.CloneURL, s.mirror); err != nil {
			return fmt.Errorf("clone: %w: %s", err, out)
		}
		// a bare clone maps heads onto heads; keep origin's branches under refs/remotes/origin
		// so run branches never collide with them.
		if out, err := gitAsDaemon(ctx, s.mirror, "", "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
			return fmt.Errorf("git config: %w: %s", err, out)
		}
	}
	// A different token or URL for the same repo: keep origin current.
	gitAsDaemon(ctx, s.mirror, "", "remote", "set-url", "origin", a.CloneURL)
	if out, err := gitAsDaemon(ctx, s.mirror, a.GitToken, "fetch", "--prune", "origin"); err != nil {
		return fmt.Errorf("fetch: %w: %s", err, out)
	}
	// a retried run starts clean
	gitAsDaemon(ctx, s.mirror, "", "worktree", "remove", "--force", dir)
	os.RemoveAll(dir)
	gitAsDaemon(ctx, s.mirror, "", "worktree", "prune")
	if out, err := gitAsDaemon(ctx, s.mirror, "", "worktree", "add", "-B", a.Branch, dir, "refs/remotes/origin/"+start); err != nil {
		return fmt.Errorf("worktree add: %w: %s", err, out)
	}
	// the agent commits into the shared object store: it must be able to write there
	return chownTree(s.mirror, id)
}

// removeWorktree drops the run's worktree and branch from the shared mirror once cleanup is set
// (the work is pushed). Failed or blocked runs keep theirs so the owner can inspect the diff.
func (s *session) removeWorktree(ctx context.Context) {
	if !s.cleanup || s.mirror == "" {
		return
	}
	unlock := s.w.lockRepo(s.mirror)
	defer unlock()
	gitAsDaemon(ctx, s.mirror, "", "worktree", "remove", "--force", s.dir)
	os.RemoveAll(s.dir)
	gitAsDaemon(ctx, s.mirror, "", "worktree", "prune")
	gitAsDaemon(ctx, s.mirror, "", "branch", "-D", s.a.Branch)
}

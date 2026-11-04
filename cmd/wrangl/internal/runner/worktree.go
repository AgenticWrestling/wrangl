package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	// A review only reads the branch: check it out detached, so it neither collides with the
	// worktree an earlier run keeps on that branch nor disturbs it.
	if a.Kind == "review" {
		if out, err := gitAsDaemon(ctx, s.mirror, "", "worktree", "add", "--detach", dir, "refs/remotes/origin/"+start); err != nil {
			return fmt.Errorf("worktree add: %w: %s", err, out)
		}
		return chownTree(s.mirror, id)
	}
	if err := w.freeBranch(ctx, s, dir); err != nil {
		return err
	}
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
	if s.a.Kind != "review" { // a review is detached: the branch belongs to the run that made it
		gitAsDaemon(ctx, s.mirror, "", "branch", "-D", s.a.Branch)
	}
}

// freeBranch makes a.Branch available for a new worktree at dir. A branch can be checked out in
// one worktree only, and an earlier run of the same task (blocked, or waiting for review, so its
// worktree was kept for inspection) still holds it: a review, revise or Smart Merge run on the same
// branch would fail with "already used by worktree". The earlier worktree is removed unless its
// run is live on this runner; its HEAD is kept under refs/wrangl/stale/<dir> first so nothing that
// was committed is lost (the run's push already put the branch on the remote). Caller holds the
// repo lock.
func (w *Worker) freeBranch(ctx context.Context, s *session, dir string) error {
	out, err := gitAsDaemon(ctx, s.mirror, "", "worktree", "list", "--porcelain")
	if err != nil {
		return nil // the add below reports any real problem
	}
	for _, blk := range strings.Split(out, "\n\n") {
		var path, head, branch string
		for _, ln := range strings.Split(blk, "\n") {
			switch {
			case strings.HasPrefix(ln, "worktree "):
				path = strings.TrimPrefix(ln, "worktree ")
			case strings.HasPrefix(ln, "HEAD "):
				head = strings.TrimPrefix(ln, "HEAD ")
			case strings.HasPrefix(ln, "branch "):
				branch = strings.TrimPrefix(ln, "branch ")
			}
		}
		if path == "" || path == dir || branch != "refs/heads/"+s.a.Branch {
			continue
		}
		if id, err := strconv.ParseInt(filepath.Base(path), 10, 64); err == nil {
			w.mu.Lock()
			_, live := w.runs[id]
			w.mu.Unlock()
			if live {
				return fmt.Errorf("branch %s is in use by live run %d on this runner", s.a.Branch, id)
			}
		}
		if head != "" {
			gitAsDaemon(ctx, s.mirror, "", "update-ref", "refs/wrangl/stale/"+filepath.Base(path), head)
		}
		s.logf("runner", "released %s from the earlier run's worktree %s", s.a.Branch, path)
		gitAsDaemon(ctx, s.mirror, "", "worktree", "remove", "--force", path)
		os.RemoveAll(path)
		gitAsDaemon(ctx, s.mirror, "", "worktree", "prune")
	}
	return nil
}

// setIdentity gives the session's worktree its own commit identity. A linked worktree shares the
// mirror's config file, so a plain `git config` from two sessions at once collides on config.lock
// (failing a run) and, when it does not, one session overwrites the other's user.name. The values
// go in the worktree's own config (extensions.worktreeConfig), and the shared file is only touched
// under the repo lock. (With that extension on, a linked worktree would otherwise read the bare
// mirror's core.bare=true, so each worktree pins core.bare=false for itself.)
func (w *Worker) setIdentity(ctx context.Context, s *session) error {
	unlock := w.lockRepo(s.mirror)
	out, err := gitAsDaemon(ctx, s.mirror, "", "config", "extensions.worktreeConfig", "true")
	unlock()
	if err != nil {
		return fmt.Errorf("git config: %w: %s", err, out)
	}
	for _, kv := range [][2]string{{"core.bare", "false"}, {"user.name", "wrangl agent (" + s.a.SessionName + ")"}, {"user.email", "agent@wrangl.invalid"}} {
		if out, err := gitAsAgent(ctx, s.id, s.dir, "config", "--worktree", kv[0], kv[1]); err != nil {
			return fmt.Errorf("git config: %w: %s", err, out)
		}
	}
	return nil
}

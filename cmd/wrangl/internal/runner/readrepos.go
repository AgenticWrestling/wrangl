package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// Read-only checkouts of the project's other repos (ADR 0012 section 6). Each is a detached worktree of the
// repo's shared mirror at <run dir>/../refs/<run id>/<name>, fetched when the run starts. Read-only is
// enforced: the files are mode a-w, the worktree is detached (no branch to push), the mirror's contents:read
// token goes to git as the daemon only and is never in the agent's environment, and before the run ends
// the daemon checks the checkout is still clean.

// readCheckout is one checkout the run made.
type readCheckout struct {
	rr     proto.ReadRepo
	path   string
	mirror string
	head   string
}

// readRoot is the directory holding this run's read-only checkouts.
func (s *session) readRoot() string {
	return filepath.Join(filepath.Dir(s.dir), "refs", strconv.FormatInt(s.a.RunID, 10))
}

// addReadRepos checks out every read repo of the assignment. A repo that cannot be fetched is skipped with
// a line in the run log: reading is a convenience and must not fail the run.
func (w *Worker) addReadRepos(ctx context.Context, s *session) {
	if len(s.a.ReadRepos) == 0 {
		return
	}
	os.RemoveAll(s.readRoot())
	for _, rr := range s.a.ReadRepos {
		c, err := w.addReadRepo(ctx, s, rr)
		if err != nil {
			s.logf("runner", "warning: no read-only checkout of %s: %v", rr.Repo, err)
			continue
		}
		s.readCheckouts = append(s.readCheckouts, c)
		s.logf("runner", "read-only checkout of %s at %s", rr.Repo, c.path)
	}
}

func (w *Worker) addReadRepo(ctx context.Context, s *session, rr proto.ReadRepo) (readCheckout, error) {
	c := readCheckout{rr: rr, path: filepath.Join(s.readRoot(), rr.Name), mirror: mirrorPath(s.id.home, rr.CloneURL)}
	if err := os.MkdirAll(filepath.Dir(c.mirror), 0o755); err != nil {
		return c, err
	}
	if err := os.MkdirAll(s.readRoot(), 0o755); err != nil {
		return c, err
	}
	unlock := w.lockRepo(c.mirror)
	defer unlock()
	if _, err := os.Stat(filepath.Join(c.mirror, "HEAD")); err != nil {
		os.RemoveAll(c.mirror)
		// Made only for reading: blobless, so the first fetch stays small. Blobs of the checked-out tree
		// come with the worktree add below.
		if out, err := gitAsDaemon(ctx, filepath.Dir(c.mirror), rr.Token, "clone", "--bare", "--filter=blob:none", rr.CloneURL, c.mirror); err != nil {
			os.RemoveAll(c.mirror)
			return c, fmt.Errorf("clone: %w: %s", err, out)
		}
		if out, err := gitAsDaemon(ctx, c.mirror, "", "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
			return c, fmt.Errorf("git config: %w: %s", err, out)
		}
	}
	gitAsDaemon(ctx, c.mirror, "", "remote", "set-url", "origin", rr.CloneURL)
	if out, err := gitAsDaemon(ctx, c.mirror, rr.Token, "fetch", "--prune", "origin"); err != nil {
		return c, fmt.Errorf("fetch: %w: %s", err, out)
	}
	gitAsDaemon(ctx, c.mirror, "", "worktree", "remove", "--force", c.path)
	os.RemoveAll(c.path)
	gitAsDaemon(ctx, c.mirror, "", "worktree", "prune")
	if out, err := gitAsDaemon(ctx, c.mirror, rr.Token, "worktree", "add", "--detach", c.path, "refs/remotes/origin/"+rr.Branch); err != nil {
		return c, fmt.Errorf("worktree add: %w: %s", err, out)
	}
	head, _ := gitAsDaemon(ctx, c.path, "", "rev-parse", "HEAD")
	c.head = head
	if err := chmodTree(c.path, false); err != nil {
		return c, err
	}
	return c, nil
}

// chmodTree makes every file and directory under root read-only (write=false) or owner-writable again.
func chmodTree(root string, write bool) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		m := info.Mode().Perm()
		if write {
			m |= 0o200
		} else {
			m &^= 0o222
		}
		return os.Chmod(p, m)
	})
}

// readWarning checks that every read-only checkout is still clean and at the commit it started on. It returns
// the warning to record on the run ("" when all is well).
func (s *session) readWarning(ctx context.Context) string {
	var bad []string
	for _, c := range s.readCheckouts {
		out, err := gitAsDaemon(ctx, c.path, "", "status", "--porcelain")
		head, _ := gitAsDaemon(ctx, c.path, "", "rev-parse", "HEAD")
		switch {
		case err != nil:
			bad = append(bad, fmt.Sprintf("%s: cannot be checked: %v", c.rr.Repo, err))
		case strings.TrimSpace(out) != "":
			n := len(strings.Split(strings.TrimSpace(out), "\n"))
			bad = append(bad, fmt.Sprintf("%s: %d path(s) changed", c.rr.Repo, n))
		case head != "" && head != c.head:
			bad = append(bad, fmt.Sprintf("%s: moved off %s", c.rr.Repo, c.head[:min(7, len(c.head))]))
		}
	}
	if len(bad) == 0 {
		return ""
	}
	w := "read-only checkout was modified (" + strings.Join(bad, "; ") + ")"
	s.logf("runner", "warning: %s", w)
	return w
}

// removeReadRepos drops the run's read-only checkouts.
func (s *session) removeReadRepos(ctx context.Context) {
	if len(s.a.ReadRepos) == 0 {
		return
	}
	for _, c := range s.readCheckouts {
		chmodTree(c.path, true)
		unlock := s.w.lockRepo(c.mirror)
		gitAsDaemon(ctx, c.mirror, "", "worktree", "remove", "--force", c.path)
		gitAsDaemon(ctx, c.mirror, "", "worktree", "prune")
		unlock()
	}
	chmodTree(s.readRoot(), true)
	os.RemoveAll(s.readRoot())
	s.readCheckouts = nil
}

// readReposEnv is the value of WRANGL_READ_REPOS: a JSON list of {repo, path, branch, note}. Never a token.
func (s *session) readReposEnv() string {
	type entry struct {
		Repo   string `json:"repo"`
		Path   string `json:"path"`
		Branch string `json:"branch"`
		Note   string `json:"note,omitempty"`
	}
	var out []entry
	for _, c := range s.readCheckouts {
		out = append(out, entry{c.rr.Repo, c.path, c.rr.Branch, c.rr.Note})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// readReposPrompt tells the agent where the other repos are and what each is.
func (s *session) readReposPrompt() string {
	if len(s.readCheckouts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Other repositories of this project are checked out READ-ONLY for you to look at (they are not yours to change; make your changes only in the repo you are working in, and name another repo in a new task if work belongs there):\n")
	for _, c := range s.readCheckouts {
		fmt.Fprintf(&b, "- %s at %s (branch %s)", c.rr.Repo, c.path, c.rr.Branch)
		if c.rr.Note != "" {
			fmt.Fprintf(&b, ": %s", c.rr.Note)
		}
		b.WriteString("\n")
	}
	b.WriteString("The same list is in the WRANGL_READ_REPOS environment variable.\n\n")
	return b.String()
}

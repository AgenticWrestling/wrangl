package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "safe.directory=*"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A branch the agent already pushed (to open its own PR) must not fail the run's push.
func TestPushBranchToleratesAgentPush(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	bare := filepath.Join(root, "o.git")
	wt := filepath.Join(root, "wt")
	git(t, root, "init", "-q", "--bare", "-b", "main", bare)
	git(t, root, "clone", "-q", bare, wt)
	os.WriteFile(filepath.Join(wt, "a"), []byte("1"), 0o644)
	git(t, wt, "add", ".")
	git(t, wt, "commit", "-qm", "base")
	git(t, wt, "push", "-q", "origin", "HEAD:main")
	w := &Worker{}
	id := agentIdentity{home: root, username: "t"}

	// Case 1: the agent pushed the very same commit; the plain push is a no-op, still fine.
	os.WriteFile(filepath.Join(wt, "b"), []byte("2"), 0o644)
	git(t, wt, "add", ".")
	git(t, wt, "commit", "-qm", "work")
	git(t, wt, "push", "-q", "origin", "HEAD:refs/heads/wrangl/x-1")
	if out, err := w.pushBranch(ctx, id, wt, "", "wrangl/x-1"); err != nil {
		t.Fatalf("same commit: %v %s", err, out)
	}

	// Case 2: the agent amended after pushing; the remote has a commit we lack. Replaced, not failed.
	git(t, wt, "commit", "-q", "--amend", "-m", "work, reworded")
	want := git(t, wt, "rev-parse", "HEAD")
	if out, err := w.pushBranch(ctx, id, wt, "", "wrangl/x-1"); err != nil {
		t.Fatalf("diverged: %v %s", err, out)
	}
	if got := git(t, bare, "rev-parse", "refs/heads/wrangl/x-1"); got != want {
		t.Fatalf("remote %s, want %s", got, want)
	}
}

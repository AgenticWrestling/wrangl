package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func gitT(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A second repo is checked out read-only beside the run: detached, mode a-w, from a blobless mirror, with the
// agent told where it is and no token anywhere the agent can see.
func TestReadOnlyCheckoutOfAnotherRepo(t *testing.T) {
	tmp := t.TempDir()
	other := filepath.Join(tmp, "other")
	gitT(t, "init", "-b", "main", other)
	gitT(t, "-C", other, "config", "uploadpack.allowFilter", "true")
	os.WriteFile(filepath.Join(other, "api.go"), []byte("package api\n"), 0o644)
	gitT(t, "-C", other, "add", ".")
	gitT(t, "-C", other, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-m", "init")

	w := &Worker{}
	id := agentIdentity{home: filepath.Join(tmp, "home")}
	a := proto.Assignment{RunID: 7, ReadRepos: []proto.ReadRepo{{Repo: "o/api", Name: "api", CloneURL: "file://" + other, Branch: "main", Note: "API server", Token: "SECRET-READ-TOKEN"}}}
	s := &session{w: w, a: a, id: id, dir: filepath.Join(id.home, "work", "7")}
	ctx := context.Background()
	w.addReadRepos(ctx, s)
	if len(s.readCheckouts) != 1 {
		t.Fatal("no read-only checkout made")
	}
	c := s.readCheckouts[0]
	if want := filepath.Join(id.home, "work", "refs", "7", "api"); c.path != want {
		t.Fatalf("path %s, want %s", c.path, want)
	}
	if b, err := os.ReadFile(filepath.Join(c.path, "api.go")); err != nil || string(b) != "package api\n" {
		t.Fatalf("file not checked out: %v %q", err, b)
	}
	if branch := gitT(t, "-C", c.path, "branch", "--show-current"); branch != "" {
		t.Fatalf("checkout is on branch %q, want detached", branch)
	}
	if filter := gitT(t, "-C", c.mirror, "config", "remote.origin.partialclonefilter"); filter != "blob:none" {
		t.Fatalf("mirror is not a blobless clone: %q", filter)
	}
	fi, _ := os.Stat(filepath.Join(c.path, "api.go"))
	if fi.Mode().Perm()&0o222 != 0 {
		t.Fatalf("file mode %v is writable", fi.Mode().Perm())
	}
	if err := os.WriteFile(filepath.Join(c.path, "api.go"), []byte("x"), 0o644); err == nil && os.Geteuid() != 0 {
		t.Fatal("wrote to a read-only checkout")
	}

	// What the agent is told: where, what, and never the token.
	env, prompt := s.readReposEnv(), s.readReposPrompt()
	for _, got := range []string{env, prompt} {
		if !strings.Contains(got, c.path) || !strings.Contains(got, "o/api") || !strings.Contains(got, "API server") || strings.Contains(got, "SECRET-READ-TOKEN") {
			t.Fatalf("agent context wrong: %s", got)
		}
	}

	// Clean: no warning. Modified: a warning that names the repo.
	if w := s.readWarning(ctx); w != "" {
		t.Fatalf("warning on a clean checkout: %s", w)
	}
	chmodTree(c.path, true)
	os.WriteFile(filepath.Join(c.path, "api.go"), []byte("changed\n"), 0o644)
	if w := s.readWarning(ctx); !strings.Contains(w, "o/api") || !strings.Contains(w, "modified") {
		t.Fatalf("warning %q", w)
	}

	// Removal works on a read-only tree and leaves the mirror.
	chmodTree(c.path, false)
	s.removeReadRepos(ctx)
	if _, err := os.Stat(s.readRoot()); err == nil {
		t.Fatal("read checkouts not removed")
	}
	if _, err := os.Stat(filepath.Join(c.mirror, "HEAD")); err != nil {
		t.Fatal("mirror lost")
	}
}

// A repo that cannot be fetched is skipped; the rest of the run is unaffected.
func TestReadOnlyCheckoutSkipsABrokenRepo(t *testing.T) {
	tmp := t.TempDir()
	w := &Worker{}
	id := agentIdentity{home: filepath.Join(tmp, "home")}
	a := proto.Assignment{RunID: 1, ReadRepos: []proto.ReadRepo{{Repo: "o/gone", Name: "gone", CloneURL: filepath.Join(tmp, "nope"), Branch: "main"}}}
	s := &session{w: w, a: a, id: id, dir: filepath.Join(id.home, "work", "1")}
	w.addReadRepos(context.Background(), s)
	if len(s.readCheckouts) != 0 || s.readReposPrompt() != "" {
		t.Fatalf("checkouts %v", s.readCheckouts)
	}
}

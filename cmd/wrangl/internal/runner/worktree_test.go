package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// Concurrent runs of one repo share a single mirror and get separate worktrees and branches.
func TestWorktreesShareMirror(t *testing.T) {
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	for _, args := range [][]string{{"init", "-b", "main", origin}, {"-C", origin, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	w := &Worker{}
	id := agentIdentity{home: filepath.Join(tmp, "home")}
	var sess []*session
	for i, br := range []string{"wr/a", "wr/b", "wr/c"} {
		a := proto.Assignment{RunID: int64(i + 1), Branch: br, CloneURL: origin}
		sess = append(sess, &session{w: w, a: a, id: id, dir: filepath.Join(id.home, "work", string(rune('1'+i)))})
	}
	var wg sync.WaitGroup
	for _, s := range sess {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.addWorktree(context.Background(), s, "main"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for _, s := range sess {
		if s.mirror != sess[0].mirror {
			t.Fatalf("mirrors differ: %s vs %s", s.mirror, sess[0].mirror)
		}
		out, err := gitAsDaemon(context.Background(), s.dir, "", "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil || out != s.a.Branch {
			t.Fatalf("%s: branch %q err %v", s.dir, out, err)
		}
		if _, err := os.Stat(filepath.Join(gitDir(s.dir))); err != nil {
			t.Fatal(err)
		}
	}
	sess[0].cleanup = true
	sess[0].removeWorktree(context.Background())
	if _, err := os.Stat(sess[0].dir); err == nil {
		t.Fatal("worktree not removed")
	}
	if _, err := os.Stat(sess[1].dir); err != nil {
		t.Fatal("sibling worktree lost")
	}
}

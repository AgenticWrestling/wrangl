package diffs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	repo           dbgen.Repo
	base, c1, head string
	svc            *Service
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	src := t.TempDir()
	run(t, src, "init", "-q", "-b", "main")
	write(t, src, "a.txt", "one\ntwo\n")
	write(t, src, "old.txt", "keep\nme\nplease\n")
	write(t, src, "gone.txt", "bye\n")
	run(t, src, "add", ".")
	run(t, src, "commit", "-qm", "base")
	base := run(t, src, "rev-parse", "HEAD")
	write(t, src, "a.txt", "one\nTWO\nthree\n")
	write(t, src, "bin.dat", "\x00\x01\x02binary")
	run(t, src, "rm", "-q", "gone.txt")
	run(t, src, "add", ".")
	run(t, src, "commit", "-qm", "first checkpoint")
	c1 := run(t, src, "rev-parse", "HEAD")
	run(t, src, "mv", "old.txt", "new.txt")
	var big strings.Builder
	for i := 0; i < 600; i++ {
		big.WriteString("line\n")
	}
	write(t, src, "big.txt", big.String())
	run(t, src, "add", ".")
	run(t, src, "commit", "-qm", "second checkpoint")
	head := run(t, src, "rev-parse", "HEAD")
	repo := dbgen.Repo{Owner: "o", Name: "r", DefaultBranch: "main", CloneUrl: "file://" + src}
	return fixture{repo, base, c1, head, &Service{Dir: t.TempDir()}}
}

func TestWholeChange(t *testing.T) {
	f := newFixture(t)
	v, err := f.svc.view(context.Background(), f.repo, f.base, f.head, "", 7)
	if err != nil {
		t.Fatal(err)
	}
	if v.Source != "mirror" || len(v.Commits) != 2 || v.Commits[0].Subject != "first checkpoint" {
		t.Fatalf("view: %+v", v)
	}
	by := map[string]int{}
	for i, x := range v.Files {
		by[x.Path] = i
	}
	a := v.Files[by["a.txt"]]
	if a.Status != "modified" || a.Additions != 2 || a.Deletions != 1 || !strings.Contains(a.Patch, "+TWO") {
		t.Errorf("a.txt: %+v", a)
	}
	if g := v.Files[by["gone.txt"]]; g.Status != "deleted" {
		t.Errorf("gone: %+v", g)
	}
	if n := v.Files[by["new.txt"]]; n.Status != "renamed" || n.OldPath != "old.txt" {
		t.Errorf("rename: %+v", n)
	}
	if b := v.Files[by["bin.dat"]]; !b.Binary || b.Patch != "" || b.Status != "added" {
		t.Errorf("binary: %+v", b)
	}
	big := v.Files[by["big.txt"]]
	if !big.Truncated || big.Patch != "" || big.Additions != 600 || !strings.HasSuffix(big.Link, "/pull/7/files") || !v.Truncated {
		t.Errorf("big: %+v", big)
	}
}

func TestOneCommit(t *testing.T) {
	f := newFixture(t)
	v, err := f.svc.view(context.Background(), f.repo, f.base, f.head, f.c1[:10], 0)
	if err != nil {
		t.Fatal(err)
	}
	if v.Commit != f.c1 || len(v.Files) != 3 || len(v.Commits) != 2 {
		t.Fatalf("view: %+v", v)
	}
	if !strings.HasSuffix(v.Link, "/commit/"+f.c1) {
		t.Errorf("link %q", v.Link)
	}
	if _, err := f.svc.view(context.Background(), f.repo, f.base, f.head, f.base, 0); !errors.Is(err, errs.NotFound) {
		t.Errorf("foreign commit: %v", err)
	}
	if _, err := f.svc.view(context.Background(), f.repo, f.base, f.head, "--output=x", 0); !errors.Is(err, errs.Invalid) {
		t.Errorf("bad commit: %v", err)
	}
}

func TestPayloadCap(t *testing.T) {
	files := []string{}
	for i := 0; i < 3; i++ {
		files = append(files, "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-x\n+"+strings.Repeat("y", MaxPayload*4/10))
	}
	d := proto.DiffView{Files: parseDiff(strings.Join(files, "\n"))}
	applyCaps(&d, gh.Repo{Owner: "o", Name: "r"}, 1)
	if d.Files[0].Truncated || d.Files[1].Truncated || !d.Files[2].Truncated {
		t.Errorf("caps: %v %v %v", d.Files[0].Truncated, d.Files[1].Truncated, d.Files[2].Truncated)
	}
}

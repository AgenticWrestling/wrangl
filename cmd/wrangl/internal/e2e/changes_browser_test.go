package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/client"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/config"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// TestChangesSectionInBrowser opens the review card's Changes section in a real browser at desktop and
// phone width (see browser/changes.js). Needs node and Playwright (PLAYWRIGHT_DIR); skipped otherwise.
func TestChangesSectionInBrowser(t *testing.T) {
	pw := os.Getenv("PLAYWRIGHT_DIR")
	if pw == "" {
		t.Skip("set PLAYWRIGHT_DIR to run the browser test")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	if _, err := controller.Init(ctx, filepath.Join(dir, "data"), filepath.Join(dir, "c.yaml")); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultController()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.Listen = config.Listen{HTTP: "127.0.0.1:0", Unix: filepath.Join(dir, "api.sock"), NATS: "127.0.0.1:-1", Enroll: "127.0.0.1:0"}
	srv, err := controller.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.LoopInterval, srv.SchedulerInterval = time.Hour, time.Hour
	go func() {
		if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("controller: %v", err)
		}
	}()
	<-srv.Ready()
	api, _ := client.New("unix://"+cfg.Listen.Unix, nil)
	if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: "lp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Fleet.DB.W.Exec(`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('tpl','','{}',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Fleet.DB.W.Exec(`INSERT INTO github_account (login, kind) VALUES ('acme', 'org')`); err != nil {
		t.Fatal(err)
	}
	if _, err := api.CreateTask(ctx, proto.TaskCreateRequest{Project: "lp", Title: "first", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	seedChanges(t, srv, dir)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				u, err := net.Dial("unix", cfg.Listen.Unix)
				if err != nil {
					c.Close()
					return
				}
				go io.Copy(u, c)
				io.Copy(c, u)
				u.Close()
				c.Close()
			}()
		}
	}()
	cmd := exec.CommandContext(ctx, node, "browser/changes.js", pw, "http://"+ln.Addr().String())
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("browser script: %v %s %s", err, out, ee.Stderr)
		}
		t.Fatalf("browser script: %v %s", err, out)
	}
	var res struct{ Errors []string }
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("bad script output %q: %v", out, err)
	}
	for _, e := range res.Errors {
		t.Error(e)
	}
}

// seedChanges inserts an open PR whose run's commits live in a local fixture repo (the clone URL), with a
// long line so the file block has to scroll sideways, and a binary file.
func seedChanges(t *testing.T, srv *controller.Server, dir string) {
	t.Helper()
	src := filepath.Join(dir, "src")
	os.MkdirAll(src, 0o755)
	g := func(args ...string) string {
		c := exec.Command("git", args...)
		c.Dir = src
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	g("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one\ntwo\n"), 0o644)
	g("add", ".")
	g("commit", "-qm", "base")
	base := g("rev-parse", "HEAD")
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("one\nTWO "+strings.Repeat("very-long-line-", 40)+"\nthree\n"), 0o644)
	os.WriteFile(filepath.Join(src, "bin.dat"), []byte("\x00\x01\x02"), 0o644)
	g("add", ".")
	g("commit", "-qm", "the work")
	head := g("rev-parse", "HEAD")
	db := srv.Fleet.DB
	for _, q := range []string{
		`INSERT INTO repo (project_id, owner, name, default_branch, clone_url) VALUES (1,'acme','app','main','file://` + src + `')`,
		`UPDATE task SET state='in_review' WHERE id=1`,
		fmt.Sprintf(`INSERT INTO run (task_id, state, repo_id, branch, base_sha, head_sha) VALUES (1,'succeeded',1,'wrangl/a','%s','%s')`, base, head),
		fmt.Sprintf(`INSERT INTO pull_request (task_id, repo_id, run_id, gh_number, url, head_branch, head_sha, mergeable) VALUES (1,1,1,7,'https://github.com/acme/app/pull/7','wrangl/a','%s','clean')`, head),
	} {
		if _, err := db.W.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

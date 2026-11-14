package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/client"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/config"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// TestMultiRepoSurfacesInBrowser drives the multi-repo owner surfaces (chips, filter, pickers, queue sheet,
// default-repo selector) at desktop and phone width (browser/repos.js). Needs node and Playwright
// (PLAYWRIGHT_DIR); skipped otherwise.
func TestMultiRepoSurfacesInBrowser(t *testing.T) {
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
	w := srv.Fleet.DB.W
	tpl := "tpl"
	for _, slug := range []string{"multi", "solo"} {
		if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: slug, Template: &tpl}); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO repo (project_id, owner, name, clone_url, note) VALUES ((SELECT id FROM project WHERE slug='multi'), 'o', 'a', 'file:///a', 'API server')`,
		`INSERT INTO repo (project_id, owner, name, clone_url, note) VALUES ((SELECT id FROM project WHERE slug='multi'), 'o', 'b', 'file:///b', 'web client')`,
		`INSERT INTO repo (project_id, owner, name, clone_url) VALUES ((SELECT id FROM project WHERE slug='solo'), 'o', 'only', 'file:///c')`,
	} {
		if _, err := w.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	need, err := api.CreateTask(ctx, proto.TaskCreateRequest{Project: "multi", Title: "no repo yet"})
	if err != nil {
		t.Fatal(err)
	}
	need2, err := api.CreateTask(ctx, proto.TaskCreateRequest{Project: "multi", Title: "also no repo"})
	if err != nil {
		t.Fatal(err)
	}
	has, err := api.CreateTask(ctx, proto.TaskCreateRequest{Project: "multi", Title: "web bit", Repo: "o/b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.CreateTask(ctx, proto.TaskCreateRequest{Project: "solo", Title: "only task"}); err != nil {
		t.Fatal(err)
	}
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
	cmd := exec.CommandContext(ctx, node, "browser/repos.js", pw, "http://"+ln.Addr().String(), need.Sqid, has.Sqid, "solo", need2.Sqid)
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
	// Queueing from the sheet with o/a chosen set it on the task and queued a run in that repo; the
	// (the script itself checks the default-repo selector sticks).
	var repo, runs int64
	w.QueryRow(`SELECT COALESCE(repo_id, 0) FROM task WHERE sqid = ?`, need.Sqid).Scan(&repo)
	w.QueryRow(`SELECT COUNT(*) FROM run r JOIN task t ON t.id = r.task_id WHERE t.sqid = ? AND r.repo_id = ?`, need.Sqid, repo).Scan(&runs)
	if repo == 0 || runs != 1 {
		t.Errorf("task repo %d, runs in it %d", repo, runs)
	}
}

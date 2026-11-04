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

// TestProjectChatInBrowser drives the project chat panel in headless Chromium at a desktop and a
// phone viewport. It needs node and Playwright (set PLAYWRIGHT_DIR to a directory whose node_modules
// has it, chromium installed) and is skipped otherwise.
func TestProjectChatInBrowser(t *testing.T) {
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
	go srv.Run(ctx)
	<-srv.Ready()
	api, _ := client.New("unix://"+cfg.Listen.Unix, nil)
	db := srv.Fleet.DB // queueing needs a runner template on the project
	if _, err := db.W.Exec(`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('go','','{}',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	type target struct{ Project, Solo string }
	targets := map[string]target{}
	for _, vp := range []string{"desktop", "phone"} { // one project each: a chat is per project and persists
		slug := "chat-" + vp
		if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: slug}); err != nil {
			t.Fatal(err)
		}
		db.W.Exec(`UPDATE project SET default_template_id = (SELECT id FROM runner_template) WHERE slug = ?`, slug)
		db.W.Exec(`INSERT INTO repo (project_id, owner, name, clone_url) VALUES ((SELECT id FROM project WHERE slug = ?), 'o', 'r', 'x')`, slug)
		var solo string
		for _, title := range []string{"Alpha job", "Alpha second", "Solo task"} {
			tk, err := api.CreateTask(ctx, proto.TaskCreateRequest{Project: slug, Title: title, Prompt: "p"})
			if err != nil {
				t.Fatal(err)
			}
			solo = tk.Sqid
		}
		targets[vp] = target{slug, solo}
	}
	tj, _ := json.Marshal(map[string]map[string]string{
		"desktop": {"project": targets["desktop"].Project, "solo": targets["desktop"].Solo},
		"phone":   {"project": targets["phone"].Project, "solo": targets["phone"].Solo},
	})
	// The browser speaks TCP; the web UI listens on the unix socket, so forward one to the other.
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
	cmd := exec.CommandContext(ctx, node, "browser/chat.js", pw, "http://"+ln.Addr().String(), string(tj))
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

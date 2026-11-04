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

// TestLivePatchKeepsClientState opens things in the browser (details, modal sheets, signals-driven
// UI), injects a probe node into #main, causes a real event so the server live-patches #main, and
// checks the probe is gone (a patch happened) while the opened state survived. Client-only state
// that a patch clobbers is the recurring bug this guards. Needs node and Playwright
// (PLAYWRIGHT_DIR, chromium installed); skipped otherwise.
func TestLivePatchKeepsClientState(t *testing.T) {
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
	if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: "lp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.CreateTask(ctx, proto.TaskCreateRequest{Project: "lp", Title: "first", Prompt: "p"}); err != nil {
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
	// The script calls back here (over a tiny HTTP hook it is given) to cause an event: it just
	// creates a task, which every page's live stream reacts to.
	hook, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hook.Close()
	n := 0
	go serveHook(hook, func() {
		n++
		api.CreateTask(ctx, proto.TaskCreateRequest{Project: "lp", Title: "bump " + string(rune('a'+n%26)), Prompt: "p"})
	})
	cmd := exec.CommandContext(ctx, node, "browser/livepatch.js", pw, "http://"+ln.Addr().String(), "http://"+hook.Addr().String())
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

// serveHook runs fn for every HTTP request on l.
func serveHook(l net.Listener, fn func()) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			buf := make([]byte, 4096)
			c.Read(buf)
			fn()
			io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nAccess-Control-Allow-Origin: *\r\nConnection: close\r\n\r\nok")
		}()
	}
}

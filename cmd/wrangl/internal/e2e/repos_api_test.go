package e2e

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/client"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/config"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// Setting a task's repo over the REST API (what `task edit --repo` and the MCP bridge use),
// on a project with two linked repos.
func TestTaskRepoAndRepoNoteOverAPI(t *testing.T) {
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
	if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: "mr"}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO repo (project_id, owner, name, clone_url) VALUES (1, 'o', 'a', 'file:///a'), (1, 'o', 'b', 'file:///b')`,
	} {
		if _, err := srv.Fleet.DB.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	tk, err := api.CreateTask(ctx, proto.TaskCreateRequest{Project: "mr", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := api.Task(ctx, tk.Sqid); d.Repo != "" || d.EffectiveRepo != "" {
		t.Fatalf("fresh task: %+v", d)
	}
	repo := "o/b"
	if _, err := api.EditTask(ctx, tk.Sqid, proto.TaskPatchRequest{Repo: &repo}); err != nil {
		t.Fatal(err)
	}
	if d, _ := api.Task(ctx, tk.Sqid); d.Repo != "o/b" || d.EffectiveRepo != "o/b" {
		t.Fatalf("after set: %+v", d)
	}
	bad := "o/nope"
	if _, err := api.EditTask(ctx, tk.Sqid, proto.TaskPatchRequest{Repo: &bad}); err == nil {
		t.Fatal("unlinked repo accepted")
	}
	empty := ""
	if _, err := api.EditTask(ctx, tk.Sqid, proto.TaskPatchRequest{Repo: &empty}); err != nil {
		t.Fatal(err)
	}
	if d, _ := api.Task(ctx, tk.Sqid); d.Repo != "" {
		t.Fatalf("after clear: %+v", d)
	}
}

package e2e

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/client"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/config"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func TestArchiveProjectEndToEnd(t *testing.T) {
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
	for _, s := range []string{"keep", "old"} {
		if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: s}); err != nil {
			t.Fatal(err)
		}
	}
	task, err := api.CreateTask(ctx, proto.TaskCreateRequest{Project: "old", Title: "old work", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	db := srv.Fleet.DB
	tk, _ := db.RQ.GetTaskBySqid(ctx, sql.NullString{String: task.Sqid, Valid: true})
	if _, err := db.Q.CreateQuestion(ctx, dbgen.CreateQuestionParams{TaskID: tk.ID, Kind: "task_review", Prompt: "Review old work?", AllowFreeText: 1, PayloadJson: "{}"}); err != nil {
		t.Fatal(err)
	}

	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", cfg.Listen.Unix)
	}}}
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := hc.Get("http://x" + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	const link = `href="/projects/old"`

	// Active: visible in the project bar, the left panel and the Inbox.
	if _, body := get("/projects/keep"); strings.Count(body, link) < 2 {
		t.Fatalf("active project missing from the project bar / left panel: %d links", strings.Count(body, link))
	}
	if _, body := get("/inbox"); !strings.Contains(body, "Review old work?") {
		t.Fatalf("question missing from the Inbox before archiving")
	}

	// A running run blocks archiving, with a message listing it.
	res, err := db.W.Exec(`INSERT INTO run (task_id, state) VALUES (?, 'running')`, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := res.LastInsertId()
	_, err = api.ArchiveProject(ctx, "old")
	var refused *client.ArchiveRefusedError
	if !errors.As(err, &refused) || len(refused.Runs) != 1 || refused.Runs[0].ID != runID || refused.Runs[0].Task != task.Sqid ||
		!strings.Contains(refused.Message, "active runs") {
		t.Fatalf("archive with a running run: %v", err)
	}
	if _, body := get("/projects/keep"); !strings.Contains(body, link) {
		t.Fatal("a refused archive hid the project")
	}

	// Once the run ends, archiving works: hidden from /projects, the left panel and /inbox.
	db.W.Exec(`UPDATE run SET state='succeeded' WHERE id=?`, runID)
	if _, err := api.ArchiveProject(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	if _, body := get("/projects/keep"); strings.Contains(body, "/projects/old") {
		t.Fatal("archived project still in the project bar or left panel")
	}
	if _, body := get("/inbox"); strings.Contains(body, "Review old work?") {
		t.Fatal("archived project's question still in the Inbox")
	}
	if ps, _ := api.Projects(ctx); len(ps) != 1 || ps[0].Slug != "keep" {
		t.Fatalf("project list: %+v", ps)
	}
	if ps, _ := api.ProjectsFiltered(ctx, "archived"); len(ps) != 1 || ps[0].Slug != "old" || !ps[0].Archived {
		t.Fatalf("archived list: %+v", ps)
	}

	// Direct URLs keep working; the project and its task are still there, but inert.
	if code, body := get("/projects/old"); code != 200 || !strings.Contains(body, "old work") {
		t.Fatalf("direct project URL: %d", code)
	}
	if code, body := get("/task/" + task.Sqid); code != 200 || !strings.Contains(body, "old work") {
		t.Fatalf("direct task URL: %d", code)
	}
	if _, err := api.CreateTask(ctx, proto.TaskCreateRequest{Project: "old", Title: "nope"}); err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("task creation in an archived project: %v", err)
	}

	// Restore brings it all back.
	if _, err := api.RestoreProject(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	if _, body := get("/projects/keep"); strings.Count(body, link) < 2 {
		t.Fatal("restored project missing from the project bar / left panel")
	}
	if _, body := get("/inbox"); !strings.Contains(body, "Review old work?") {
		t.Fatal("restored project's question missing from the Inbox")
	}
	if _, err := api.CreateTask(ctx, proto.TaskCreateRequest{Project: "old", Title: "again"}); err != nil {
		t.Fatalf("create after restore: %v", err)
	}
}

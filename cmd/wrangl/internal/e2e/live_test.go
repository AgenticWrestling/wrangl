package e2e

import (
	"bufio"
	"context"
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
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// sseReader reads Datastar patch events off a live stream.
type sseReader struct {
	rd     *bufio.Reader
	frames chan string
}

func openLive(t *testing.T, hc *http.Client, ctx context.Context, query string) *sseReader {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://x/ui/live?"+query, nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	s := &sseReader{rd: bufio.NewReader(resp.Body), frames: make(chan string, 64)}
	go func() {
		defer resp.Body.Close()
		var b strings.Builder
		for {
			line, err := s.rd.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\n" {
				if b.Len() > 0 {
					s.frames <- b.String()
				}
				b.Reset()
				continue
			}
			b.WriteString(line)
		}
	}()
	return s
}

// mainFrame waits for the next #main re-render for which ok holds, and returns it.
func (s *sseReader) mainFrame(t *testing.T, what string, ok func(string) bool) string {
	t.Helper()
	deadline := time.After(6 * time.Second)
	var last string
	for {
		select {
		case f := <-s.frames:
			if strings.Contains(f, `<main id="main">`) {
				last = f
				if ok(f) {
					return f
				}
			}
		case <-deadline:
			var keep []string
			for _, l := range strings.Split(last, "\n") {
				if strings.Contains(l, `class="detail"`) || strings.Contains(l, "subtasks") {
					keep = append(keep, strings.TrimSpace(l))
				}
			}
			t.Fatalf("live stream never showed %s; last #main frame (%d bytes), detail lines: %q", what, len(last), keep)
		}
	}
}

// Regression (task i4Ahwa): a run moving queued -> assigned -> running must move its card from
// Queue to In progress on the live stream, in every project, and a card the page has expanded
// must stay in the stream whichever section it moves to.
func TestLiveStreamFollowsRunStatesAndKeepsExpandedCard(t *testing.T) {
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
	srv.LoopInterval, srv.SchedulerInterval = time.Hour, time.Hour // nothing assigns behind the test's back
	go srv.Run(ctx)
	<-srv.Ready()
	api, _ := client.New("unix://"+cfg.Listen.Unix, nil)
	e := &workEnv{ctx: ctx, api: api, t: t, db: srv.Fleet.DB}
	for _, slug := range []string{"web", "api"} {
		if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: slug}); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(project, title, parent string) string {
		tk, err := api.CreateTask(ctx, proto.TaskCreateRequest{Project: project, Title: title, Prompt: "p " + title, Parent: parent})
		if err != nil {
			t.Fatal(err)
		}
		return tk.Sqid
	}
	big := mk("web", "big job", "")
	leaf := mk("web", "leaf a", big)
	mk("web", "leaf b", big)
	solo := mk("web", "solo", "")
	other := mk("api", "other project job", "")

	var runner int64
	if r, err := e.db.W.Exec(`INSERT INTO runner (name, template_id, state) VALUES ('r1', 1, 'busy')`); err == nil {
		runner, _ = r.LastInsertId()
	}
	runFor := func(sqid string) int64 {
		res, err := e.db.W.Exec(`INSERT INTO run (task_id) SELECT id FROM task WHERE sqid = ?`, sqid)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	setState := func(id int64, state string) {
		q := `UPDATE run SET state = ? WHERE id = ?`
		if state != "queued" && runner != 0 {
			q = `UPDATE run SET state = ?, runner_id = ` + "(SELECT id FROM runner WHERE name='r1')" + ` WHERE id = ?`
		}
		if _, err := e.db.W.Exec(q, state, id); err != nil {
			t.Fatal(err)
		}
		srv.Bus.Publish("run", "x") // what the controller does when a run changes
	}
	rBig, rSolo, rOther := runFor(big), runFor(solo), runFor(other)

	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", cfg.Listen.Unix)
	}}}
	lctx, lcancel := context.WithTimeout(ctx, 40*time.Second)
	defer lcancel()
	live := openLive(t, hc, lctx, "view=tasks&project=web&sid=livesid")
	has := func(sec, sqid string) func(string) bool {
		return func(f string) bool {
			i := strings.Index(f, `id="`+sec+`"`)
			if i < 0 {
				return false
			}
			end := len(f)
			for _, next := range []string{`id="queue"`, `id="backlog"`} {
				if j := strings.Index(f[i+1:], next); j >= 0 && i+1+j < end && next != `id="`+sec+`"` {
					end = i + 1 + j
				}
			}
			return strings.Contains(f[i:end], `data-sqid="`+sqid+`"`)
		}
	}
	if f := live.mainFrame(t, "big job in Queue", has("queue", big)); strings.Contains(f, `id="in-progress"`) {
		t.Fatalf("nothing runs yet: %.600s", f)
	}

	// Expand the queued card, then let the run be assigned and start running.
	if r, err := hc.Post("http://x/ui/expand?sid=livesid&sqid="+big, "text/plain", nil); err != nil {
		t.Fatal(err)
	} else {
		r.Body.Close()
	}
	withCard := func(sec string) func(string) bool {
		return func(f string) bool { return has(sec, big)(f) && strings.Contains(f, `id="subtasks"`) }
	}
	live.mainFrame(t, "the expanded card inside Queue", withCard("queue"))
	setState(rOther, "queued") // another project's run: must not disturb this page's sections
	setState(rBig, "assigned")
	live.mainFrame(t, "big job under In progress after assignment, still expanded", withCard("in-progress"))
	setState(rBig, "running")
	live.mainFrame(t, "big job under In progress while running, still expanded", withCard("in-progress"))
	// A second run queues behind it while the first runs; the expanded card is untouched.
	setState(rSolo, "queued")
	live.mainFrame(t, "solo in Queue and big still expanded in In progress", func(f string) bool {
		return withCard("in-progress")(f) && has("queue", solo)(f)
	})
	// Expand a card that is below a run entry (a subtask), then move the run on.
	if r, err := hc.Post("http://x/ui/expand?sid=livesid&sqid="+leaf, "text/plain", nil); err == nil {
		r.Body.Close()
	}
	live.mainFrame(t, "the subtask card expanded beneath its run entry", func(f string) bool {
		return strings.Contains(f, `id="d-`+leaf+`"`) && strings.Contains(f, `id="subtasks"`)
	})
	setState(rSolo, "assigned")
	live.mainFrame(t, "both runs under In progress", func(f string) bool { return has("in-progress", solo)(f) && has("in-progress", big)(f) })
}

// Regression (task i4Ahwa): the "In progress" chip view lists leaf tasks directly; a card expanded
// there must stay in the live stream too, or the next patch collapses it.
func TestLiveStreamKeepsCardExpandedInNowView(t *testing.T) {
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
	if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: "web"}); err != nil {
		t.Fatal(err)
	}
	par, _ := api.CreateTask(ctx, proto.TaskCreateRequest{Project: "web", Title: "parent", Prompt: "p"})
	kid, _ := api.CreateTask(ctx, proto.TaskCreateRequest{Project: "web", Title: "working leaf", Prompt: "p", Parent: par.Sqid})
	srv.Fleet.DB.W.Exec(`UPDATE task SET state='in_progress' WHERE sqid = ?`, kid.Sqid)

	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", cfg.Listen.Unix)
	}}}
	lctx, lcancel := context.WithTimeout(ctx, 20*time.Second)
	defer lcancel()
	live := openLive(t, hc, lctx, "view=tasks&project=web&now=1&sid=nowsid")
	live.mainFrame(t, "the leaf in the In progress view", func(f string) bool { return strings.Contains(f, `data-sqid="`+kid.Sqid+`"`) })
	if r, err := hc.Get("http://x/ui/detail?sqid=" + kid.Sqid); err != nil || r.StatusCode != 200 {
		b := []byte{}
		if r != nil {
			b, _ = io.ReadAll(r.Body)
		}
		t.Fatalf("detail: %v %s", err, b)
	}
	if r, err := hc.Post("http://x/ui/expand?sid=nowsid&sqid="+kid.Sqid, "text/plain", nil); err == nil {
		r.Body.Close()
	}
	live.mainFrame(t, "the expanded card in the In progress view", func(f string) bool {
		return strings.Contains(f, `id="d-`+kid.Sqid+`"`) && strings.Contains(f, `id="subtasks"`)
	})
}

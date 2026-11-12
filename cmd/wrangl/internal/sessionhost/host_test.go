package sessionhost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"context"
)

// TestMain doubles as the fake child process: tests start this binary with SESSIONHOST_FAKE=1.
// It prints its arguments and a banner, reports SIGWINCH with the PTY size, and obeys commands
// typed to it: "echo X", "exit N", "flood", "query" (asks the terminal for its cursor position).
func TestMain(m *testing.M) {
	if os.Getenv("SESSIONHOST_FAKE") == "1" {
		fakeChild()
		return
	}
	os.Exit(m.Run())
}

func fakeChild() {
	ch := make(chan os.Signal, 8)
	signal.Notify(ch, syscall.SIGWINCH)
	fmt.Printf("FAKE args=%s\r\n", strings.Join(os.Args[1:], ","))
	go func() {
		for range ch {
			sz, err := pty.GetsizeFull(os.Stdin)
			if err == nil {
				fmt.Printf("WINCH %dx%d\r\n", sz.Cols, sz.Rows)
			}
		}
	}()
	buf := make([]byte, 4096)
	var line []byte
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return
		}
		for _, c := range buf[:n] {
			if c != '\r' && c != '\n' {
				line = append(line, c)
				continue
			}
			cmd := string(line)
			line = line[:0]
			switch {
			case strings.HasPrefix(cmd, "echo "):
				fmt.Printf("ECHO %s\r\n", cmd[5:])
			case strings.HasPrefix(cmd, "exit "):
				var code int
				fmt.Sscan(cmd[5:], &code)
				os.Exit(code)
			case cmd == "flood":
				chunk := strings.Repeat("x", 1000) + "\r\n"
				for i := 0; i < 3000; i++ {
					fmt.Print(chunk)
				}
				fmt.Print("FLOODDONE\r\n")
			case cmd == "query":
				fmt.Print("\x1b[6n")
			}
		}
	}
}

func fakeCfg() Config {
	return Config{
		Argv: func(sp Spec, resume bool) []string {
			a := []string{os.Args[0], "name=" + sp.Name}
			if resume {
				a = append(a, "resume")
			}
			return a
		},
		Env:       []string{"SESSIONHOST_FAKE=1"},
		StopGrace: time.Second,
	}
}

type env struct {
	h  *Host
	c  Client
	t  *testing.T
	sp string
}

func newEnv(t *testing.T, mod func(*Config)) *env {
	t.Helper()
	cfg := fakeCfg()
	if mod != nil {
		mod(&cfg)
	}
	h := NewHost(cfg)
	dir, err := os.MkdirTemp("", "sh")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		(&Server{Host: h, Path: sock, Group: "no-such-group-here", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Serve(ctx)
		close(done)
	}()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { h.StopAll(); cancel(); <-done; os.RemoveAll(dir) })
	return &env{h: h, c: Client{Path: sock}, t: t, sp: t.TempDir()}
}

func (e *env) start(slug string) Info {
	e.t.Helper()
	r, err := e.c.Do(Request{Op: OpStart, Slug: slug, Dir: e.sp})
	if err != nil {
		e.t.Fatal(err)
	}
	return *r.Session
}

// reader collects an attached client's output on a goroutine.
type reader struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	end  *int
	size [2]int
	a    *Attached
}

func attach(t *testing.T, c Client, req Request) *reader {
	t.Helper()
	a, err := c.Attach(req)
	if err != nil {
		t.Fatal(err)
	}
	r := &reader{a: a}
	t.Cleanup(func() { a.Detach() })
	go func() {
		for {
			typ, p, err := a.Next()
			if err != nil {
				return
			}
			r.mu.Lock()
			switch typ {
			case FrameOutput:
				r.buf.Write(p)
			case FrameSize:
				var s struct{ Cols, Rows int }
				json.Unmarshal(p, &s)
				r.size = [2]int{s.Cols, s.Rows}
			case FrameEnd:
				var e struct {
					ExitCode int `json:"exit_code"`
				}
				json.Unmarshal(p, &e)
				r.end = &e.ExitCode
			}
			r.mu.Unlock()
		}
	}()
	return r
}

func (r *reader) wait(t *testing.T, what string, ok func(out string, r *reader) bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		good := ok(r.buf.String(), r)
		r.mu.Unlock()
		if good {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t.Fatalf("timed out waiting for %s; got %q", what, r.buf.String())
}

func contains(s string) func(string, *reader) bool {
	return func(out string, _ *reader) bool { return strings.Contains(out, s) }
}

func TestStartListIdempotentAndSocketMode(t *testing.T) {
	e := newEnv(t, nil)
	i := e.start("proj")
	if i.State != "running" || i.PID == 0 || i.Name != "proj" || i.Resumed {
		t.Fatalf("%+v", i)
	}
	r, _ := e.c.Do(Request{Op: OpStart, Slug: "proj", Dir: e.sp})
	if !r.Existing || r.Session.PID != i.PID {
		t.Fatalf("second start: %+v", r)
	}
	l, err := e.c.Do(Request{Op: OpList})
	if err != nil || len(l.Sessions) != 1 || l.Sessions[0].Slug != "proj" {
		t.Fatalf("%+v %v", l, err)
	}
	st, _ := os.Stat(e.c.Path)
	if st.Mode().Perm() != 0o660 {
		t.Errorf("socket mode %v", st.Mode().Perm())
	}
	for _, bad := range []Request{
		{Op: OpStart, Slug: "Bad Slug", Dir: e.sp},
		{Op: OpStart, Slug: "x", Dir: "relative"},
		{Op: OpStart, Slug: "x", Dir: "/definitely/not/there"},
		{Op: OpStop, Slug: "nope"},
		{Op: OpAttach, Slug: "nope"},
		{Op: "bogus"},
	} {
		if _, err := e.c.Do(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestAttachReplayLiveInputAndDetach(t *testing.T) {
	e := newEnv(t, nil)
	info := e.start("p")
	a := attach(t, e.c, Request{Slug: "p", Control: true, Cols: 100, Rows: 30})
	a.wait(t, "banner", contains("FAKE args=name=p"))
	a.wait(t, "size frame", func(_ string, r *reader) bool { return r.size == [2]int{100, 30} })
	a.a.Input([]byte("echo hi\r"))
	a.wait(t, "echo", contains("ECHO hi"))

	// A second, watch-only client gets the screen first, then live output; its input is ignored.
	w := attach(t, e.c, Request{Slug: "p"})
	w.wait(t, "snapshot with earlier output", contains("ECHO hi"))
	w.a.Input([]byte("echo sneaky\r"))
	a.a.Input([]byte("echo two\r"))
	w.wait(t, "live output", contains("ECHO two"))
	a.wait(t, "own live output", contains("ECHO two"))
	if strings.Contains(w.buf.String(), "sneaky") || strings.Contains(a.buf.String(), "sneaky") {
		t.Error("a watch-only client typed into the session")
	}
	l, _ := e.c.Do(Request{Op: OpList})
	if l.Sessions[0].Clients != 2 {
		t.Errorf("clients = %d", l.Sessions[0].Clients)
	}

	// Detaching never kills it.
	a.a.Detach()
	w.a.Detach()
	time.Sleep(100 * time.Millisecond)
	l, _ = e.c.Do(Request{Op: OpList})
	if l.Sessions[0].State != "running" || l.Sessions[0].PID != info.PID || l.Sessions[0].Clients != 0 {
		t.Fatalf("after detach: %+v", l.Sessions[0])
	}
	b := attach(t, e.c, Request{Slug: "p"})
	b.wait(t, "replay after reattach", contains("ECHO two"))
}

func TestResizeFromControllingClientAndWinchOnAttach(t *testing.T) {
	e := newEnv(t, nil)
	e.start("p")
	attach(t, e.c, Request{Slug: "p"}).wait(t, "child ready", contains("FAKE"))
	a := attach(t, e.c, Request{Slug: "p", Control: true, Cols: 90, Rows: 25})
	a.wait(t, "winch from the size change", contains("WINCH 90x25"))
	// A watch-only client may not resize.
	w := attach(t, e.c, Request{Slug: "p", Cols: 50, Rows: 10})
	w.a.Resize(50, 10)
	time.Sleep(150 * time.Millisecond)
	// The latest controlling client wins and everyone is told.
	c2 := attach(t, e.c, Request{Slug: "p", Control: true})
	c2.a.Resize(70, 20)
	w.wait(t, "size frame", func(_ string, r *reader) bool { return r.size == [2]int{70, 20} })
	a.wait(t, "winch 70x20", contains("WINCH 70x20"))
	// Reattaching at an unchanged size still sends SIGWINCH so the app repaints.
	before := strings.Count(a.buf.String(), "WINCH 70x20")
	attach(t, e.c, Request{Slug: "p", Control: true, Cols: 70, Rows: 20})
	a.wait(t, "winch on attach", func(out string, _ *reader) bool { return strings.Count(out, "WINCH 70x20") > before })
	if i := e.h.List()[0]; i.Cols != 70 || i.Rows != 20 {
		t.Errorf("%+v", i)
	}
	if _, err := e.c.Attach(Request{Slug: "p", Control: true, Cols: 5000, Rows: 5}); err == nil {
		t.Error("absurd size accepted")
	}
}

func TestRingReplay(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.RingBytes = 4096 })
	e.start("p")
	a := attach(t, e.c, Request{Slug: "p", Control: true})
	a.wait(t, "banner", contains("FAKE"))
	for i := 0; i < 400; i++ { // overflows the 4 KiB ring
		a.a.Input([]byte(fmt.Sprintf("echo line-%03d\r", i)))
	}
	a.wait(t, "last echo", contains("ECHO line-399"))
	r := attach(t, e.c, Request{Slug: "p", Replay: "ring"})
	r.wait(t, "ring tail", contains("ECHO line-399"))
	r.mu.Lock()
	out := r.buf.String()
	r.mu.Unlock()
	if strings.Contains(out, "FAKE args") || len(out) > 5000 {
		t.Errorf("ring not bounded: %d bytes, has banner: %v", len(out), strings.Contains(out, "FAKE args"))
	}
}

func TestExitStopRestart(t *testing.T) {
	e := newEnv(t, nil)
	first := e.start("p")
	a := attach(t, e.c, Request{Slug: "p", Control: true})
	a.wait(t, "banner", contains("FAKE"))
	a.a.Input([]byte("exit 3\r"))
	a.wait(t, "end frame", func(_ string, r *reader) bool { return r.end != nil && *r.end == 3 })
	deadline := time.Now().Add(3 * time.Second)
	for e.h.List()[0].State != "exited" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if i := e.h.List()[0]; i.State != "exited" || i.ExitCode != 3 {
		t.Fatalf("%+v", i)
	}
	// Attaching to an ended session still shows its last screen, then the end.
	b := attach(t, e.c, Request{Slug: "p"})
	b.wait(t, "last screen and end", func(out string, r *reader) bool { return strings.Contains(out, "FAKE") && r.end != nil })

	// start again resumes with -c.
	i := e.start("p")
	if i.State != "running" || !i.Resumed || i.PID == first.PID {
		t.Fatalf("restart via start: %+v", i)
	}
	c := attach(t, e.c, Request{Slug: "p"})
	c.wait(t, "resume arg", contains("args=name=p,resume"))

	// restart replaces a running process, clients are ended.
	r, err := e.c.Do(Request{Op: OpRestart, Slug: "p"})
	if err != nil || r.Session.PID == i.PID || !r.Session.Resumed {
		t.Fatalf("restart: %+v %v", r, err)
	}
	c.wait(t, "end for the old client", func(_ string, r *reader) bool { return r.end != nil })
	if err := syscall.Kill(i.PID, 0); err == nil {
		// zombie-free: the old process must be gone
		t.Error("old process still alive after restart")
	}

	if _, err := e.c.Do(Request{Op: OpStop, Slug: "p"}); err != nil {
		t.Fatal(err)
	}
	if l := e.h.List(); len(l) != 0 {
		t.Errorf("stopped session still listed: %+v", l)
	}
}

func TestSlowClientDroppedSessionUnaffected(t *testing.T) {
	old := outFrames
	outFrames = 64
	defer func() { outFrames = old }()
	e := newEnv(t, nil)
	e.start("p")
	ctl := attach(t, e.c, Request{Slug: "p", Control: true})
	ctl.wait(t, "banner", contains("FAKE"))
	slow, err := e.c.Attach(Request{Slug: "p"}) // never reads
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Detach()
	ctl.a.Input([]byte("flood\r"))
	// The slow client is dropped (its connection closes) while the session keeps running.
	deadline := time.Now().Add(8 * time.Second)
	for e.h.List()[0].Clients > 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if i := e.h.List()[0]; i.State != "running" || i.Clients > 1 {
		t.Fatalf("slow client not dropped / session hurt: %+v", i)
	}
	// The session is still usable: a fresh attach gets a snapshot and live output.
	f := attach(t, e.c, Request{Slug: "p", Control: true})
	f.a.Input([]byte("echo after\r"))
	f.wait(t, "output after the flood", contains("ECHO after"))
}

func TestQueryAnswersReachTheApp(t *testing.T) {
	// The emulator answers the app's cursor-position query; without draining its Read side the app
	// would wait forever. The fake prints what it reads back as an ECHO.
	e := newEnv(t, func(c *Config) {
		c.Argv = func(sp Spec, resume bool) []string {
			return []string{"/bin/sh", "-c", `printf 'ready\r\n'; printf '\033[6n'; IFS=R read -r -d R x; printf 'GOT[%s]\r\n' "$(printf %s "$x" | od -An -c | tr -d ' \n')"; sleep 5`}
		}
	})
	e.start("p")
	a := attach(t, e.c, Request{Slug: "p"})
	a.wait(t, "reply delivered to the app", contains("GOT["))
}

func TestHostSurvivesClientsAndServerRestartIsExplicit(t *testing.T) {
	// Sessions belong to the Host, not to a connection or a Server: a new Server over the same Host
	// serves the same live session.
	e := newEnv(t, nil)
	i := e.start("p")
	h := e.h
	sock := filepath.Join(t.TempDir(), "again.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&Server{Host: h, Path: sock, Group: "no-such-group-here", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Serve(ctx)
	for k := 0; k < 100; k++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	l, err := Client{Path: sock}.Do(Request{Op: OpList})
	if err != nil || len(l.Sessions) != 1 || l.Sessions[0].PID != i.PID {
		t.Fatalf("%+v %v", l, err)
	}
	// A second server on a live socket refuses.
	if err := (&Server{Host: h, Path: sock}).Serve(ctx); err == nil {
		t.Error("two servers on one socket")
	}
}

func TestStartPreparesCheckoutAndResumeFlag(t *testing.T) {
	run := func(dir string, args ...string) {
		t.Helper()
		out, err := git(context.Background(), dir, "", args...)
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	src := t.TempDir()
	run(src, "init", "-q", "-b", "trunk")
	run(src, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "one")
	e := newEnv(t, nil)
	dir := filepath.Join(t.TempDir(), "work", "primary", "p")
	r, err := e.c.Do(Request{Op: OpStart, Slug: "p", Dir: dir, CloneURL: src, Branch: "trunk", Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("no checkout: %v", err)
	}
	if !r.Session.Resumed {
		t.Error("Resume not honoured for a session the host has no record of")
	}
	// A clean checkout fast-forwards on the next prepare; a dirty one is left alone.
	run(src, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "two")
	if err := prepareCheckout(dir, src, "trunk", ""); err != nil {
		t.Fatal(err)
	}
	log, _ := git(context.Background(), dir, "", "log", "--format=%s")
	if !strings.Contains(log, "two") {
		t.Errorf("not fast-forwarded: %q", log)
	}
	os.WriteFile(filepath.Join(dir, "scratch"), []byte("x"), 0o644)
	run(dir, "add", "scratch")
	run(src, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "three")
	prepareCheckout(dir, src, "trunk", "")
	if log, _ := git(context.Background(), dir, "", "log", "--format=%s"); strings.Contains(log, "three") {
		t.Error("a dirty checkout was moved")
	}
	if err := prepareCheckout(filepath.Join(t.TempDir(), "x"), "/no/such/repo", "trunk", ""); err == nil {
		t.Error("clone failure not reported")
	}
}

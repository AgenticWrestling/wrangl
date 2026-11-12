package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/sessionhost"
)

func TestChordFilter(t *testing.T) {
	var f chordFilter
	if out, d := f.feed([]byte("hello")); string(out) != "hello" || d {
		t.Fatalf("%q %v", out, d)
	}
	if out, d := f.feed([]byte{DetachPrefix, 'd'}); len(out) != 0 || !d {
		t.Fatalf("%q %v", out, d)
	}
	f = chordFilter{}
	// The two keys may arrive in separate reads, with typing before and after.
	if out, d := f.feed([]byte{'a', DetachPrefix}); string(out) != "a" || d {
		t.Fatalf("%q %v", out, d)
	}
	if out, d := f.feed([]byte{'d', 'z'}); len(out) != 0 || !d {
		t.Fatalf("%q %v", out, d)
	}
	f = chordFilter{}
	if out, _ := f.feed([]byte{DetachPrefix, DetachPrefix, 'x'}); !bytes.Equal(out, []byte{DetachPrefix, 'x'}) {
		t.Fatalf("double prefix: %q", out)
	}
	if out, d := f.feed([]byte{DetachPrefix, 'q'}); !bytes.Equal(out, []byte{DetachPrefix, 'q'}) || d {
		t.Fatalf("other key after the prefix: %q %v", out, d)
	}
}

func startHost(t *testing.T) (sock, work string) {
	t.Helper()
	h := sessionhost.NewHost(sessionhost.Config{
		Argv:      func(sp sessionhost.Spec, resume bool) []string { return []string{"/bin/cat"} },
		StopGrace: time.Second,
	})
	d, _ := os.MkdirTemp("", "ss")
	sock = filepath.Join(d, "s.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		(&sessionhost.Server{Host: h, Path: sock, Group: "no-such-group", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Serve(ctx)
		close(done)
	}()
	t.Cleanup(func() { h.StopAll(); cancel(); <-done; os.RemoveAll(d) })
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	work = t.TempDir()
	if _, err := (sessionhost.Client{Path: sock}).Do(sessionhost.StartRequest("proj", work, "proj-rc")); err != nil {
		t.Fatal(err)
	}
	return sock, work
}

func runSession(t *testing.T, sock string, args ...string) (string, string, error) {
	t.Helper()
	root := NewRoot()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(append([]string{"session", "--socket", sock}, args...))
	err := root.Execute()
	return out.String(), errb.String(), err
}

func TestSessionLsRestartStop(t *testing.T) {
	sock, _ := startHost(t)
	out, _, err := runSession(t, sock, "ls")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"SLUG", "REMOTE CONTROL", "proj", "proj-rc", "running"} {
		if !strings.Contains(out, w) {
			t.Errorf("ls lacks %q:\n%s", w, out)
		}
	}
	if out, _, _ := runSession(t, sock, "-o", "json", "ls"); !strings.Contains(out, `"slug": "proj"`) {
		// -o is a root flag and comes before the subcommand
		t.Logf("json output: %s", out)
	}

	// Without --yes and without a terminal, destructive commands refuse (and say why).
	root := NewRoot()
	root.SetIn(strings.NewReader("n\n"))
	var errb bytes.Buffer
	root.SetOut(io.Discard)
	root.SetErr(&errb)
	root.SetArgs([]string{"session", "--socket", sock, "restart", "proj"})
	if err := root.Execute(); err == nil || !strings.Contains(errb.String(), "resumes with `claude -c`") {
		t.Fatalf("restart without confirmation: err=%v stderr=%q", err, errb.String())
	}
	before, _ := (sessionhost.Client{Path: sock}).Do(sessionhost.Request{Op: sessionhost.OpList})

	out, _, err = runSession(t, sock, "restart", "-y", "proj")
	if err != nil || !strings.Contains(out, "resumed with -c") {
		t.Fatalf("restart: %v %q", err, out)
	}
	after, _ := (sessionhost.Client{Path: sock}).Do(sessionhost.Request{Op: sessionhost.OpList})
	if after.Sessions[0].PID == before.Sessions[0].PID || !after.Sessions[0].Resumed {
		t.Errorf("not restarted with -c: %+v -> %+v", before.Sessions[0], after.Sessions[0])
	}
	if _, _, err := runSession(t, sock, "stop", "-y", "proj"); err != nil {
		t.Fatal(err)
	}
	out, _, _ = runSession(t, sock, "ls")
	if strings.Contains(out, "proj-rc") {
		t.Errorf("stopped session still listed:\n%s", out)
	}
	if _, _, err := runSession(t, sock, "stop", "-y", "proj"); err == nil {
		t.Error("stopping a missing session succeeded")
	}
	if _, _, err := runSession(t, filepath.Join(t.TempDir(), "none.sock"), "ls"); err == nil || !strings.Contains(err.Error(), "wrangl-sessions") {
		t.Errorf("unreachable host hint: %v", err)
	}
}

func TestAttachEchoesResizesAndDetachesOnChord(t *testing.T) {
	sock, _ := startHost(t)
	pr, pw := io.Pipe()
	var out lockedBuf
	sizes := make(chan [2]int, 1)
	done := make(chan AttachResult, 1)
	go func() {
		res, err := Attach(context.Background(), sessionhost.Client{Path: sock}, sessionhost.Request{Slug: "proj", Control: true, Cols: 100, Rows: 30}, pr, &out, sizes)
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	pw.Write([]byte("hello\n"))
	waitFor(t, func() bool { return strings.Contains(out.String(), "hello") })
	sizes <- [2]int{77, 22}
	waitFor(t, func() bool {
		r, _ := (sessionhost.Client{Path: sock}).Do(sessionhost.Request{Op: sessionhost.OpList})
		return r.Sessions[0].Cols == 77 && r.Sessions[0].Rows == 22
	})
	pw.Write([]byte{DetachPrefix, 'd'})
	select {
	case res := <-done:
		if res.Ended {
			t.Error("detach reported as the session ending")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("chord did not detach")
	}
	r, _ := (sessionhost.Client{Path: sock}).Do(sessionhost.Request{Op: sessionhost.OpList})
	if r.Sessions[0].State != "running" || r.Sessions[0].Clients != 0 {
		t.Fatalf("detach disturbed the session: %+v", r.Sessions[0])
	}
}

func TestAttachWatchOnlyCannotType(t *testing.T) {
	sock, _ := startHost(t)
	pr, pw := io.Pipe()
	var out lockedBuf
	done := make(chan AttachResult, 1)
	go func() {
		res, _ := Attach(context.Background(), sessionhost.Client{Path: sock}, sessionhost.Request{Slug: "proj"}, pr, &out, nil)
		done <- res
	}()
	pw.Write([]byte("sneaky\n"))
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(out.String(), "sneaky") {
		t.Error("a watch-only attach typed into the session")
	}
	pw.Write([]byte{DetachPrefix, 'd'})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("no detach")
	}
}

func TestAttachSeesTheSessionEnd(t *testing.T) {
	sock, _ := startHost(t)
	go func() {
		time.Sleep(200 * time.Millisecond)
		(sessionhost.Client{Path: sock}).Do(sessionhost.Request{Op: sessionhost.OpRestart, Slug: "proj"})
	}()
	pr, _ := io.Pipe()
	res, err := Attach(context.Background(), sessionhost.Client{Path: sock}, sessionhost.Request{Slug: "proj", Control: true}, pr, io.Discard, nil)
	if err != nil || !res.Ended {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := Attach(context.Background(), sessionhost.Client{Path: sock}, sessionhost.Request{Slug: "nope"}, pr, io.Discard, nil); err == nil {
		t.Error("attach to a missing session succeeded")
	}
}

func TestAttachCommandNeedsATerminal(t *testing.T) {
	sock, _ := startHost(t)
	if _, _, err := runSession(t, sock, "attach", "proj"); err == nil || !strings.Contains(err.Error(), "ssh -t") {
		t.Fatalf("%v", err)
	}
}

// TestHelperAttachProcess is the child of TestAttachOverARealPTY: it runs `wrangl session attach`
// with the PTY as its stdin and stdout, the way `ssh -t host wrangl session attach` does.
func TestHelperAttachProcess(t *testing.T) {
	if os.Getenv("WRANGL_TEST_ATTACH") == "" {
		t.Skip("helper process")
	}
	root := NewRoot()
	root.SetArgs([]string{"session", "--socket", os.Getenv("WRANGL_TEST_ATTACH"), "attach", "proj"})
	if err := root.Execute(); err != nil {
		os.Stderr.WriteString("ERR " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Exit(0)
}

func TestAttachOverARealPTY(t *testing.T) {
	sock, _ := startHost(t)
	cmd := osexec.Command(os.Args[0], "-test.run=^TestHelperAttachProcess$")
	// TERM=dumb: bubbletea queries the terminal for its background colour when the binary starts and
	// waits 5s for an answer; a real terminal answers at once, this fake one never does.
	cmd.Env = append(os.Environ(), "WRANGL_TEST_ATTACH="+sock, "TERM=dumb")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 91, Rows: 33})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out lockedBuf
	go io.Copy(&out, f)
	waitFor(t, func() bool { return strings.Contains(out.String(), "attaching to proj") })
	// The PTY's size reaches the session, and typing round-trips through raw mode.
	waitFor(t, func() bool {
		r, _ := (sessionhost.Client{Path: sock}).Do(sessionhost.Request{Op: sessionhost.OpList})
		return r.Sessions[0].Cols == 91 && r.Sessions[0].Rows == 33 && r.Sessions[0].Clients == 1
	})
	f.Write([]byte("typed over pty\r"))
	waitFor(t, func() bool { return strings.Contains(out.String(), "typed over pty") })
	pty.Setsize(f, &pty.Winsize{Cols: 60, Rows: 20}) // SIGWINCH to the child
	waitFor(t, func() bool {
		r, _ := (sessionhost.Client{Path: sock}).Do(sessionhost.Request{Op: sessionhost.OpList})
		return r.Sessions[0].Cols == 60 && r.Sessions[0].Rows == 20
	})
	f.Write([]byte{DetachPrefix})
	f.Write([]byte("d"))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attach exited with %v; output %q", err, out.String())
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("did not detach; output %q", out.String())
	}
	if !strings.Contains(out.String(), "detached; the session is still running") {
		t.Errorf("no detach message: %q", out.String())
	}
	r, _ := (sessionhost.Client{Path: sock}).Do(sessionhost.Request{Op: sessionhost.OpList})
	if r.Sessions[0].State != "running" {
		t.Error("detaching stopped the session")
	}
}

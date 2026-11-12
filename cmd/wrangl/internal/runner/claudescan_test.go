package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

type fakeProc struct {
	pid, ppid int
	comm      string
	args      []string
	env       []string
	cwd       string
}

func writeProcs(t *testing.T, ps ...fakeProc) string {
	t.Helper()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "stat"), []byte("cpu 1 2 3\nbtime 1000000\n"), 0o644)
	for _, p := range ps {
		d := filepath.Join(root, fmt.Sprint(p.pid))
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "comm"), []byte(p.comm+"\n"), 0o644)
		os.WriteFile(filepath.Join(d, "cmdline"), []byte(strings.Join(p.args, "\x00")+"\x00"), 0o644)
		os.WriteFile(filepath.Join(d, "environ"), []byte(strings.Join(p.env, "\x00")+"\x00"), 0o644)
		// fields after "(comm)": state ppid ... utime(14) stime(15) ... starttime(22) vsize rss(24)
		f := make([]string, 22)
		for i := range f {
			f[i] = "0"
		}
		f[0], f[1] = "S", fmt.Sprint(p.ppid)
		f[11], f[12] = "300", "50" // fields 14, 15
		f[19] = "500"              // field 22
		f[21] = "10"               // field 24
		os.WriteFile(filepath.Join(d, "stat"), []byte(fmt.Sprintf("%d (%s) %s\n", p.pid, p.comm+") x (", strings.Join(f, " "))), 0o644)
		if p.cwd != "" {
			os.Symlink(p.cwd, filepath.Join(d, "cwd"))
		}
	}
	return root
}

func TestScanClaudeKinds(t *testing.T) {
	root := writeProcs(t,
		fakeProc{pid: 10, ppid: 1, comm: "wrangl", args: []string{"/usr/bin/wrangl", "runner"}},
		fakeProc{pid: 11, ppid: 10, comm: "sh", args: []string{"sh"}, env: []string{"WRANGL_RUN_ID=42", "WRANGL_TASK=abc123"}},
		fakeProc{pid: 12, ppid: 11, comm: "claude", args: []string{"claude", "-p"}, cwd: "/work/a"},
		fakeProc{pid: 20, ppid: 1, comm: "wrangl", args: []string{"/usr/bin/wrangl", "session-host"}},
		fakeProc{pid: 21, ppid: 20, comm: "claude", args: []string{"claude", "--remote-control", "proj", "--name", "proj"}, cwd: "/work/p"},
		fakeProc{pid: 30, ppid: 1, comm: "fish", args: []string{"fish"}, env: []string{"WRANGL_MISE=/x"}},
		fakeProc{pid: 31, ppid: 30, comm: "node", args: []string{"node", "/opt/bin/claude"}},
		fakeProc{pid: 40, ppid: 1, comm: "claude", args: []string{"claude", "--remote-control=hand"}, env: []string{"ZELLIJ_SESSION_NAME=z1"}},
		fakeProc{pid: 41, ppid: 1, comm: "claude", args: []string{"claude"}, env: []string{"TMUX=/tmp/tmux-1/default,123,0"}},
		fakeProc{pid: 50, ppid: 1, comm: "bash", args: []string{"bash"}},
	)
	got := ScanClaude(root, time.Now())
	by := map[int]proto.ClaudeProc{}
	for _, c := range got {
		by[c.PID] = c
	}
	if len(got) != 5 {
		t.Fatalf("want 5 claude procs, got %+v", got)
	}
	for pid, kind := range map[int]string{12: "run", 21: "primary", 31: "shell", 40: "other", 41: "other"} {
		if by[pid].Kind != kind {
			t.Errorf("pid %d kind = %q, want %q", pid, by[pid].Kind, kind)
		}
	}
	if r := by[12]; r.RunID != 42 || r.Task != "abc123" || r.Cwd != "/work/a" {
		t.Errorf("run: %+v", r)
	}
	if by[21].RCName != "proj" || by[40].RCName != "hand" {
		t.Errorf("rc names: %q %q", by[21].RCName, by[40].RCName)
	}
	if by[40].Mux != "zellij:z1" || by[41].Mux != "tmux:/tmp/tmux-1/default" {
		t.Errorf("mux: %q %q", by[40].Mux, by[41].Mux)
	}
	c := by[12]
	if c.CPUSecs != 3.5 || c.RSS != 10*uint64(os.Getpagesize()) {
		t.Errorf("cpu/rss: %v %v", c.CPUSecs, c.RSS)
	}
	if want := time.Unix(1000005, 0).UTC(); !c.Started.Equal(want) {
		t.Errorf("started %v want %v", c.Started, want)
	}
}

func TestScanClaudeMissingRoot(t *testing.T) {
	if got := ScanClaude(filepath.Join(t.TempDir(), "nope"), time.Now()); got != nil {
		t.Fatalf("got %v", got)
	}
}

func TestHeartbeatClaudeRoundTrip(t *testing.T) {
	hb := proto.Heartbeat{Claude: []proto.ClaudeProc{{PID: 1, Kind: proto.ClaudeOther}}}
	b, _ := json.Marshal(hb)
	var back proto.Heartbeat
	if err := json.Unmarshal(b, &back); err != nil || len(back.Claude) != 1 || back.Claude[0].Kind != "other" {
		t.Fatalf("%v %+v", err, back)
	}
	// An old runner's heartbeat has no field and still decodes.
	if err := json.Unmarshal([]byte(`{"version":"x"}`), &back); err != nil {
		t.Fatal(err)
	}
}

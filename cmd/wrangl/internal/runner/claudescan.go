package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

const (
	clkTck       = 100 // USER_HZ is 100 on every Linux ABI we run on
	maxClaudeRep = 100
)

// ScanClaude lists the Claude processes under procRoot (normally /proc). It reads only comm,
// cmdline, stat, cwd, environ and the ppid chain: no ptrace, no hooks. Fields it cannot read
// (another user's environ or cwd) are left empty.
func ScanClaude(procRoot string, now time.Time) []proto.ClaudeProc {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	btime := bootTime(procRoot)
	var out []proto.ClaudeProc
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(procRoot, e.Name())
		args := cmdline(dir)
		if !isClaude(readTrim(filepath.Join(dir, "comm")), args) {
			continue
		}
		st, ok := readStat(dir)
		if !ok {
			continue
		}
		c := proto.ClaudeProc{PID: pid, RSS: st.rssPages * uint64(os.Getpagesize()), CPUSecs: float64(st.cpuTicks) / clkTck}
		if !btime.IsZero() {
			c.Started = btime.Add(time.Duration(st.startTicks) * time.Second / clkTck).UTC()
		}
		c.Cwd, _ = os.Readlink(filepath.Join(dir, "cwd"))
		c.RCName = rcName(args)
		env := environ(dir)
		c.Mux = muxOf(env)
		c.Kind, c.RunID, c.Task = classify(procRoot, pid, env)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	if len(out) > maxClaudeRep {
		out = out[:maxClaudeRep]
	}
	return out
}

func isClaude(comm string, args []string) bool {
	if comm == "claude" {
		return true
	}
	if len(args) > 0 && filepath.Base(args[0]) == "claude" {
		return true
	}
	// node /path/to/claude ...
	return len(args) > 1 && filepath.Base(args[0]) == "node" && filepath.Base(args[1]) == "claude"
}

// classify walks the ppid chain (the process first) in the ADR's order: run, primary, shell, other.
func classify(procRoot string, pid int, own map[string]string) (kind string, runID int64, task string) {
	var primary, shell bool
	env := own
	for i := 0; pid > 1 && i < 64; i++ {
		dir := filepath.Join(procRoot, strconv.Itoa(pid))
		if i > 0 {
			env = environ(dir)
		}
		if v := env["WRANGL_RUN_ID"]; v != "" {
			id, _ := strconv.ParseInt(v, 10, 64)
			return proto.ClaudeRun, id, env["WRANGL_TASK"]
		}
		if _, ok := env["WRANGL_MISE"]; ok {
			shell = true
		}
		if i > 0 && isSessionHost(cmdline(dir)) {
			primary = true
		}
		st, ok := readStat(dir)
		if !ok {
			break
		}
		pid = st.ppid
	}
	switch {
	case primary:
		return proto.ClaudePrimary, 0, ""
	case shell:
		return proto.ClaudeShell, 0, ""
	}
	return proto.ClaudeOther, 0, ""
}

func isSessionHost(args []string) bool {
	if len(args) < 2 || !strings.HasPrefix(filepath.Base(args[0]), "wrangl") {
		return false
	}
	for _, a := range args[1:] {
		if a == "session-host" {
			return true
		}
	}
	return false
}

func rcName(args []string) string {
	for i, a := range args {
		for _, f := range []string{"--remote-control", "--rc"} {
			if a == f && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return args[i+1]
			}
			if strings.HasPrefix(a, f+"=") {
				return strings.TrimPrefix(a, f+"=")
			}
		}
	}
	return ""
}

func muxOf(env map[string]string) string {
	if v := env["ZELLIJ_SESSION_NAME"]; v != "" {
		return "zellij:" + v
	}
	if v := env["TMUX"]; v != "" {
		return "tmux:" + strings.SplitN(v, ",", 2)[0]
	}
	return ""
}

type procStat struct {
	ppid                           int
	cpuTicks, startTicks, rssPages uint64
}

// readStat parses /proc/<pid>/stat; comm may contain spaces and parens, so split after the last ')'.
func readStat(dir string) (procStat, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return procStat{}, false
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return procStat{}, false
	}
	f := strings.Fields(string(b[i+1:])) // f[0] is field 3 (state)
	if len(f) < 22 {
		return procStat{}, false
	}
	u := func(n int) uint64 { v, _ := strconv.ParseUint(f[n-3], 10, 64); return v }
	return procStat{ppid: int(u(4)), cpuTicks: u(14) + u(15), startTicks: u(22), rssPages: u(24)}, true
}

func bootTime(procRoot string) time.Time {
	b, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		return time.Time{}
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "btime "); ok {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return time.Unix(n, 0).UTC()
		}
	}
	return time.Time{}
}

func readTrim(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

func cmdline(dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil || len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
}

func environ(dir string) map[string]string {
	b, err := os.ReadFile(filepath.Join(dir, "environ"))
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, kv := range strings.Split(string(b), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

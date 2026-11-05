package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func startSocket(t *testing.T) (string, *Worker) {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0o755)
	os.Chmod(filepath.Dir(dir), 0o755) // let another uid reach the socket, so the uid check is what decides
	w := &Worker{Cfg: &Config{StateDir: dir, AgentHome: dir}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	path := filepath.Join(dir, "runner.sock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.ServeAgentSocket(ctx, path)
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return path, w
}

func TestMCPShimListsToolsAndRelays(t *testing.T) {
	path, _ := startSocket(t)
	t.Setenv("WRANGL_SOCK", path)
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"task_get","arguments":{"sqid":"x"}}}`,
	}, "\n")
	var out bytes.Buffer
	if err := ServeMCP(context.Background(), strings.NewReader(in), &out, "test"); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	// No run kind yet: only the read tools are offered.
	if !strings.Contains(s, `"task_get"`) || strings.Contains(s, `"task_create"`) {
		t.Fatalf("tools/list: %s", s)
	}
	// With no run in progress the daemon refuses, and the model sees a tool error.
	if !strings.Contains(s, `"isError":true`) || !strings.Contains(s, "no run in progress") {
		t.Fatalf("tools/call: %s", s)
	}
}

func TestSocketRejectsOtherUsers(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("needs root to connect as another user")
	}
	path, _ := startSocket(t)
	curl := func(uid uint32) string {
		cmd := exec.Command("curl", "-sS", "--unix-socket", path, "http://x/tools")
		if uid != 0 {
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: uid}}
		}
		b, _ := cmd.CombinedOutput()
		return string(b)
	}
	os.Chmod(path, 0o666) // filesystem permissions alone would let nobody in
	if got := curl(65534); !strings.Contains(got, "only the agent user") {
		t.Fatalf("nobody was let in: %q", got)
	}
	var tools []map[string]any
	if err := json.Unmarshal([]byte(curl(0)), &tools); err != nil || len(tools) == 0 {
		t.Fatalf("own uid refused: %v", err)
	}
}

// The daemon lists the run's custom tool declarations after the stack tools, and refuses a name
// the run was not given before it costs a round trip.
func TestShimListsCustomToolDeclarations(t *testing.T) {
	path, w := startSocket(t)
	t.Setenv("WRANGL_SOCK", path)
	t.Setenv("WRANGL_RUN_ID", "5")
	w.mu.Lock()
	w.runs = map[int64]*runState{5: {id: 5, kind: "work", tools: []proto.ToolDecl{
		{Name: "ext_deploy_status", Description: "Show the deployment status", InputSchema: json.RawMessage(`{"type":"object","properties":{"env":{"type":"string","enum":["staging","prod"]}}}`), RunKinds: []string{"work"}}}}}
	w.mu.Unlock()
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ext_other","arguments":{}}}`,
	}, "\n")
	var out bytes.Buffer
	if err := ServeMCP(context.Background(), strings.NewReader(in), &out, "test"); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, `"ext_deploy_status"`) || !strings.Contains(s, `"task_create"`) || !strings.Contains(s, `"enum":["staging","prod"]`) {
		t.Fatalf("tools/list: %s", s)
	}
	if !strings.Contains(s, "unknown tool ext_other") {
		t.Fatalf("unknown custom tool: %s", s)
	}
}

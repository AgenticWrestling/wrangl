package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

var wantPaths = []string{
	"controller init", "controller serve", "controller migrate", "controller backup",
	"controller secret set", "controller secret list", "controller secret rm",
	"runner bootstrap", "runner run", "runner hook", "runner mcp", "runner status",
	"tui",
	"project create", "project list", "project show", "project edit", "project delete",
	"repo link", "repo unlink", "repo list",
	"issue sync", "issue list", "issue show", "issue to-task",
	"task create", "task tree", "task list", "task show", "task edit", "task move",
	"task needs add", "task needs rm", "task split", "task queue",
	"task complete", "task reopen", "task cancel", "task delete",
	"run list", "run show", "run cancel", "run attach", "run diff", "run files", "run cat",
	"logs search",
	"pr list", "pr show", "pr approve", "pr request-changes", "pr close",
	"question list", "question show", "question answer",
	"template create", "template list", "template show", "template edit", "template delete",
	"fleet list", "fleet show", "fleet launch", "fleet cordon", "fleet uncordon",
	"fleet drain", "fleet terminate", "fleet enroll-token",
	"push test",
	"config use", "config show",
}

func find(t *testing.T, path string) *cobra.Command {
	t.Helper()
	c, rest, err := NewRoot().Find(strings.Fields(path))
	if err != nil || len(rest) != 0 || c.CommandPath() != "wrangl "+path {
		t.Fatalf("%q: got %q rest=%v err=%v", path, c.CommandPath(), rest, err)
	}
	return c
}

func TestTreeComplete(t *testing.T) {
	for _, p := range wantPaths {
		if c := find(t, p); c.Short == "" {
			t.Errorf("%q has no Short", p)
		}
	}
	for _, p := range []string{"runner hook", "runner mcp"} {
		if !find(t, p).Hidden {
			t.Errorf("%q should be hidden", p)
		}
	}
}

// run executes the CLI with args, isolated from the real home and environment.
func run(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("WRANGL_CONFIG", "")
	t.Setenv("WRANGL_CONTROLLER", "")
	root := NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestOutputFlagValidated(t *testing.T) {
	if _, err := run(t, "", "config", "show", "-o", "bogus"); err == nil || !strings.Contains(err.Error(), "--output") {
		t.Fatalf("got %v", err)
	}
}

func TestStubsNameTheirMilestone(t *testing.T) {
	cases := map[string][]string{
		"wrangl task create: not implemented yet (milestone M4)":   {"task", "create", "proj", "--title", "x"},
		"wrangl logs: not implemented yet (milestone M4)":          {"logs", "42"}, // logs <run> itself, not a subcommand
		"wrangl question list: not implemented yet (milestone M5)": {"question", "list"},
		"wrangl tui: not implemented yet (milestone M7)":           {"tui"},
	}
	for want, args := range cases {
		_, err := run(t, "", args...)
		if !errors.Is(err, ErrNotImplemented) || err.Error() != want {
			t.Errorf("%v: got %v", args, err)
		}
	}
}

func TestArgAndFlagValidation(t *testing.T) {
	bad := [][]string{
		{"task", "move", "abc123", "--first", "--last"},        // positions exclusive
		{"task", "move", "abc123", "--root", "--parent", "x"},  // root vs parent
		{"task", "create", "p", "--prompt", "a", "-f", "b.md"}, // prompt vs file
		{"task", "split", "abc123", "--agent", "--title", "a"}, // agent vs manual
		{"task", "show"},                      // missing sqid
		{"repo", "link", "proj"},              // missing repo
		{"controller", "migrate", "sideways"}, // invalid direction
	}
	for _, args := range bad {
		if _, err := run(t, "", args...); err == nil || errors.Is(err, ErrNotImplemented) {
			t.Errorf("%v: want a validation error, got %v", args, err)
		}
	}
}

func TestConfigUseAndShow(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "client.yaml")
	if _, err := run(t, "", "--config", cfg, "config", "use", "https://wrangl.example.com"); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "", "--config", cfg, "config", "show", "-o", "json")
	if err != nil || !strings.Contains(out, `"controller": "https://wrangl.example.com"`) {
		t.Fatalf("%q %v", out, err)
	}
	out, _ = run(t, "", "--config", cfg, "--controller", "unix:///tmp/x.sock", "config", "show")
	if !strings.Contains(out, "unix:///tmp/x.sock") {
		t.Fatalf("--controller did not override: %q", out)
	}
}

func TestControllerLifecycleCommands(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	cfg := filepath.Join(dir, "controller.yaml")
	c := func(stdin string, args ...string) string {
		t.Helper()
		out, err := run(t, stdin, append([]string{"--config", cfg}, args...)...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	if out := c("", "controller", "init", "--data-dir", data); !strings.Contains(out, "wrote config") {
		t.Fatalf("init: %q", out)
	}
	if out := c("", "controller", "init"); !strings.Contains(out, "kept config") {
		t.Fatalf("re-init should keep config: %q", out)
	}
	if out := c("", "controller", "migrate", "status"); !strings.Contains(out, "00001_init.sql") || !strings.Contains(out, "applied") {
		t.Fatalf("migrate status: %q", out)
	}
	c("tok-123\n", "controller", "secret", "set", "claude_oauth_token")
	if out := c("", "controller", "secret", "list"); strings.TrimSpace(out) != "claude_oauth_token" {
		t.Fatalf("secret list: %q", out)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "secrets", "claude_oauth_token")); string(b) != "tok-123" {
		t.Fatalf("secret value %q", b)
	}
	c("", "controller", "secret", "rm", "claude_oauth_token")
	bk := filepath.Join(dir, "backup.db")
	c("", "controller", "backup", bk)
	if fi, err := os.Stat(bk); err != nil || fi.Size() == 0 {
		t.Fatalf("backup: %v", err)
	}
	if _, err := run(t, "", "--config", cfg, "controller", "backup", bk); err == nil {
		t.Fatal("backup overwrote an existing file")
	}
}

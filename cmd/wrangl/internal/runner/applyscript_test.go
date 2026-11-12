package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner/runnertest"
)

// The rendered script must do what Applier.Apply does: same mise config content, mise run as
// the user in the user's home, setup script run once and again only when it changes.
func TestApplyScriptMatchesApplier(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin", "mise")
	if err := runnertest.MiseInstaller(nil)(context.Background(), bin, os.Stderr); err != nil {
		t.Fatal(err)
	}
	home, state, marker := filepath.Join(root, "home"), filepath.Join(root, "state"), filepath.Join(root, "setup-runs")
	os.MkdirAll(home, 0o755)
	tmpl := proto.TemplateApply{
		MiseTOML:    "[tools]\ngo = \"1.26\"\nnode = 'it''s'\n",
		SetupScript: "echo run >> " + marker + "\ncat <<'EOF'\nnested heredoc\nEOF\n",
	}
	opts := ScriptOptions{MiseBin: bin, StateDir: state, Home: home, NoSudo: true}
	run := func(tm proto.TemplateApply) string {
		cmd := exec.Command("bash", "-c", ApplyScript(tm, "tester", opts))
		cmd.Env = append(os.Environ(), "HOME="+home, "USER=tester")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return string(out)
	}

	out := run(tmpl)
	got, _ := os.ReadFile(filepath.Join(home, ".config/mise/conf.d/wrangl-dev.toml"))
	if string(got) != tmpl.MiseTOML {
		t.Fatalf("mise config %q", got)
	}
	calls, _ := os.ReadFile(filepath.Join(home, "mise-calls.log"))
	if !strings.Contains(string(calls), "install --yes HOME="+home+" USER=tester") { // same shape the Applier test checks
		t.Fatalf("mise calls %q", calls)
	}
	if !strings.Contains(out, "nested heredoc") || !strings.Contains(out, "mise: all tools installed") {
		t.Fatalf("output:\n%s", out)
	}
	if out = run(tmpl); !strings.Contains(out, "setup script unchanged") {
		t.Fatalf("second run did not skip setup:\n%s", out)
	}
	if runs, _ := os.ReadFile(marker); strings.Count(string(runs), "run") != 1 {
		t.Fatalf("setup ran %q", runs)
	}
	tmpl.SetupScript += "# changed\n"
	run(tmpl)
	if runs, _ := os.ReadFile(marker); strings.Count(string(runs), "run") != 2 {
		t.Fatalf("changed setup did not rerun: %q", runs)
	}
}

func TestApplyScriptSetupFailureStopsBeforeMise(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "mise")
	runnertest.MiseInstaller(nil)(context.Background(), bin, os.Stderr)
	opts := ScriptOptions{MiseBin: bin, StateDir: filepath.Join(root, "s"), Home: root, NoSudo: true}
	cmd := exec.Command("bash", "-c", ApplyScript(proto.TemplateApply{MiseTOML: "[tools]\n", SetupScript: "false"}, "u", opts))
	cmd.Env = append(os.Environ(), "HOME="+root)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("want failure, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(root, "mise-calls.log")); err == nil {
		t.Fatal("mise must not run after a failed setup script")
	}
	if _, err := os.Stat(filepath.Join(root, "s", "setup.sha256")); err == nil {
		t.Fatal("a failed setup must not be recorded as done")
	}
}

func TestApplyScriptQuoting(t *testing.T) {
	s := ApplyScript(proto.TemplateApply{}, "bob'; rm -rf /", ScriptOptions{})
	if !strings.Contains(s, `TARGET_USER='bob'\''; rm -rf /'`) {
		t.Fatal(s)
	}
}

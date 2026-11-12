package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every command here names its paths under a temp dir: nothing may touch /etc/claude-code.
func runPolicy(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"policy"}, args...))
	err := root.Execute()
	return out.String(), err
}

func TestPolicyInstallIsADryRunByDefault(t *testing.T) {
	dir := t.TempDir()
	managed := filepath.Join(dir, "managed.json")
	wt := filepath.Join(dir, "primary", "demo")
	os.MkdirAll(wt, 0o755)
	out, err := runPolicy(t, "install", "--managed-path", managed, "--worktree", wt)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"managed\t" + managed + ": would write", "primary\t" + filepath.Join(wt, ".claude", "settings.json") + ": would write", "dry run: nothing was written"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in\n%s", w, out)
		}
	}
	if _, err := os.Stat(managed); err == nil {
		t.Fatal("dry run wrote the managed file")
	}
	if _, err := os.Stat(filepath.Join(wt, ".claude")); err == nil {
		t.Fatal("dry run wrote the worktree file")
	}
}

func TestPolicyInstallApplyForceAndShow(t *testing.T) {
	dir := t.TempDir()
	wt := filepath.Join(dir, "primary", "demo")
	os.MkdirAll(wt, 0o755)
	// Worktree only (no root needed); managed settings are skipped.
	out, err := runPolicy(t, "install", "--no-managed", "--worktree", wt, "--apply")
	if err != nil || !strings.Contains(out, ": wrote") {
		t.Fatalf("%v\n%s", err, out)
	}
	file := filepath.Join(wt, ".claude", "settings.json")
	b, _ := os.ReadFile(file)
	if !strings.Contains(string(b), "Edit("+wt+"/**)") || !strings.Contains(string(b), "Agent(general-purpose)") {
		t.Errorf("content: %s", b)
	}
	os.WriteFile(file, []byte(`{"edited":true}`), 0o644)
	out, _ = runPolicy(t, "install", "--no-managed", "--worktree", wt, "--apply")
	if !strings.Contains(out, "not overwritten") {
		t.Errorf("%s", out)
	}
	if b, _ := os.ReadFile(file); string(b) != `{"edited":true}` {
		t.Fatal("overwritten without --force")
	}
	out, _ = runPolicy(t, "show", "--no-managed", "--worktree", wt)
	if !strings.Contains(out, "in force, differs from the defaults") || !strings.Contains(out, `"edited":true`) {
		t.Errorf("show: %s", out)
	}
	if _, err := runPolicy(t, "install", "--no-managed", "--worktree", wt, "--apply", "--force"); err != nil {
		t.Fatal(err)
	}
	out, _ = runPolicy(t, "show", "--no-managed", "--worktree", wt)
	if !strings.Contains(out, "identical to the defaults") {
		t.Errorf("show after force: %s", out)
	}
	out, _ = runPolicy(t, "show", "--managed-path", filepath.Join(dir, "none.json"), "--no-managed=false", "--worktree", filepath.Join(dir, "other"))
	if strings.Count(out, "not present") != 2 {
		t.Errorf("absent files: %s", out)
	}
}

func TestPolicyShowDefaults(t *testing.T) {
	for _, k := range []string{"managed", "primary", "run"} {
		out, err := runPolicy(t, "show", "--defaults", k)
		if err != nil || !strings.Contains(out, `"permissions"`) {
			t.Errorf("%s: %v %q", k, err, out)
		}
	}
	if out, err := runPolicy(t, "show", "--defaults", "primary"); err != nil || !strings.Contains(out, "<worktree>") {
		t.Errorf("placeholders: %v %q", err, out)
	}
	if _, err := runPolicy(t, "show", "--defaults", "nope"); err == nil {
		t.Error("unknown kind")
	}
}

func TestPolicyInstallNeedsSomethingToDo(t *testing.T) {
	if _, err := runPolicy(t, "install", "--no-managed"); err == nil {
		t.Error("an empty install succeeded")
	}
}

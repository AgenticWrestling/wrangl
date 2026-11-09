package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wcli "github.com/agenticwrestling/wrangl/cmd/wrangl/internal/cli"
)

func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := wcli.NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestDevCommandsExist(t *testing.T) {
	root := wcli.NewRoot()
	for _, p := range [][]string{
		{"dev", "list"}, {"dev", "connect"}, {"dev", "ssh"}, {"dev", "ssh-config"}, {"dev", "tunnel"},
		{"dev", "start"}, {"dev", "stop"}, {"dev", "setup"}, {"dev", "proxy"}, {"fleet", "connect"}, {"fleet", "tunnel"},
	} {
		if c, _, err := root.Find(p); err != nil || c == root || c.Name() != p[len(p)-1] {
			t.Errorf("command %v missing: %v", p, err)
		}
	}
}

func TestDevSetupDryRunDefault(t *testing.T) {
	out, err := runCLI(t, "dev", "setup", "--dry-run", "--user", "bob", "--with", "browser", "--idle-stop", "2h")
	if err != nil {
		t.Fatal(err, out)
	}
	for _, want := range []string{"TARGET_USER='bob'", "aws-cli", "java", "ec2-instance-connect", "playwright", "7200", "mise install"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q", want)
		}
	}
}

func TestDevSetupDryRunFileAndErrors(t *testing.T) {
	f := filepath.Join(t.TempDir(), "p.yaml")
	os.WriteFile(f, []byte("mise_toml: |\n  [tools]\n  zig = \"latest\"\nsetup_script: echo custom\n"), 0o644)
	out, err := runCLI(t, "dev", "setup", "--dry-run", "--file", f)
	if err != nil || !strings.Contains(out, "zig") || !strings.Contains(out, "echo custom") || strings.Contains(out, "aws-cli") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := runCLI(t, "dev", "setup", "--dry-run", "--file", f, "--template", "x"); err == nil {
		t.Error("--file with --template must fail")
	}
	if _, err := runCLI(t, "dev", "setup", "--dry-run", "--with", "nope"); err == nil {
		t.Error("an unknown overlay must fail")
	}
}

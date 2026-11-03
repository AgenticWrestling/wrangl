package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestShellPreludeInstallsOnce(t *testing.T) {
	home := t.TempDir()
	mise := filepath.Join(home, "mise")
	os.WriteFile(mise, []byte("#!/bin/sh\necho fake-mise \"$@\" >> \""+home+"/calls\"\n"), 0o755)
	run := func() string {
		cmd := exec.Command("/bin/sh", "-c", shellPrelude, "wrangl-shell", "/bin/sh")
		cmd.Env = []string{"HOME=" + home, "WRANGL_MISE=" + mise, "PATH=/usr/bin:/bin"}
		cmd.Stdin = strings.NewReader("")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return string(out)
	}
	if out := run(); !strings.Contains(out, "Setting up shell environment") || !strings.Contains(out, "ready") {
		t.Fatalf("first shell output: %q", out)
	}
	cfg, _ := os.ReadFile(filepath.Join(home, ".config/mise/conf.d/wrangl-shell.toml"))
	for _, tool := range []string{"bat", "lsd", "dust", "duf", "fd", "ripgrep", "tealdeer"} {
		if !strings.Contains(string(cfg), tool+" = ") {
			t.Errorf("config missing %s: %s", tool, cfg)
		}
	}
	if out := run(); strings.Contains(out, "Setting up") {
		t.Fatalf("second shell reinstalled: %q", out)
	}
	calls, _ := os.ReadFile(filepath.Join(home, "calls"))
	if strings.Count(string(calls), "install --yes") != 1 {
		t.Fatalf("mise calls: %q", calls)
	}
}

func TestLookPathIn(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "claude"), []byte("#!/bin/sh\n"), 0o755)
	if p, ok := lookPathIn("claude", "/nonexistent:"+d); !ok || p != filepath.Join(d, "claude") {
		t.Fatalf("got %q %v", p, ok)
	}
	if _, ok := lookPathIn("nope", d); ok {
		t.Fatal("found a missing binary")
	}
}

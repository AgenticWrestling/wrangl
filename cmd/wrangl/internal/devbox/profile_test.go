package devbox_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/devbox"
)

func TestDefaultProfile(t *testing.T) {
	p := devbox.Default()
	for _, want := range []string{"aws-cli", "go =", "rust", "java = \"corretto-25\"", "ripgrep", "tealdeer"} {
		if !strings.Contains(p.MiseTOML, want) {
			t.Errorf("default toolchain lacks %q", want)
		}
	}
	if strings.Count(p.MiseTOML, "[tools]") != 1 {
		t.Fatalf("one [tools] table expected:\n%s", p.MiseTOML)
	}
	if !strings.Contains(p.SetupScript, "ec2-instance-connect") {
		t.Error("default setup must install ec2-instance-connect")
	}
	if len(strings.Split(strings.TrimSpace(p.SetupScript), "\n")) > 10 {
		t.Error("the OS setup must stay minimal")
	}
}

func TestMergeOverlays(t *testing.T) {
	p, err := devbox.Default().Merge("android", "browser")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(p.MiseTOML, "[tools]") != 1 || !strings.Contains(p.MiseTOML, "android-sdk") || !strings.Contains(p.MiseTOML, "npm:playwright") {
		t.Fatalf("bad merge:\n%s", p.MiseTOML)
	}
	if !strings.Contains(p.SetupScript, "ec2-instance-connect") || !strings.Contains(p.SetupScript, "libxkbcommon") {
		t.Fatalf("setup scripts not concatenated:\n%s", p.SetupScript)
	}
	if _, err := devbox.Default().Merge("nope"); err == nil {
		t.Fatal("unknown overlay must fail")
	}
}

// The merged config must be valid TOML as far as mise is concerned (skipped without mise).
func TestMergedConfigParsesWithMise(t *testing.T) {
	mise, err := exec.LookPath("mise")
	if err != nil {
		t.Skip("mise not installed")
	}
	p, _ := devbox.Default().Merge("android", "browser", "fish")
	dir := t.TempDir()
	cfg := filepath.Join(dir, "mise.toml")
	os.WriteFile(cfg, []byte(p.MiseTOML), 0o644)
	cmd := exec.Command(mise, "ls", "--missing")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "MISE_CONFIG_DIR="+dir, "MISE_DATA_DIR="+dir, "MISE_CACHE_DIR="+dir, "MISE_TRUSTED_CONFIG_PATHS="+dir)
	if out, err := cmd.CombinedOutput(); err != nil && strings.Contains(string(out), "parse") {
		t.Fatalf("mise could not parse the config: %v\n%s", err, out)
	}
}

func TestLoadFile(t *testing.T) {
	d := t.TempDir()
	good := filepath.Join(d, "p.yaml")
	os.WriteFile(good, []byte("mise_toml: |\n  [tools]\n  go = \"1.26\"\nsetup_script: echo hi\nname: ignored-template-field\n"), 0o644)
	p, err := devbox.LoadFile(good)
	if err != nil || !strings.Contains(p.MiseTOML, "go =") || p.SetupScript != "echo hi" {
		t.Fatalf("%+v %v", p, err)
	}
	empty := filepath.Join(d, "e.yaml")
	os.WriteFile(empty, []byte("name: x\n"), 0o644)
	if _, err := devbox.LoadFile(empty); err == nil {
		t.Fatal("a file with neither field must fail")
	}
	if _, err := devbox.LoadFile(filepath.Join(d, "none")); err == nil {
		t.Fatal("missing file must fail")
	}
}

func TestIdleStopScript(t *testing.T) {
	s := devbox.IdleStopScript(4 * time.Hour)
	if !strings.Contains(s, "14400") || !strings.Contains(s, "systemctl poweroff") || strings.Contains(s, "aws ") {
		t.Fatalf("unexpected script:\n%s", s)
	}
	if out, err := exec.Command("bash", "-n", "-c", s).CombinedOutput(); err != nil {
		t.Fatalf("syntax: %v\n%s", err, out)
	}
}

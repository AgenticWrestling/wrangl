package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nreid/devhost/cmd/wrangl/internal/proto"
	"github.com/nreid/devhost/cmd/wrangl/internal/runner/runnertest"
)

func newApplier(t *testing.T) (*Applier, *Config, *int, string) {
	t.Helper()
	root := t.TempDir()
	cfg := &Config{
		StateDir: filepath.Join(root, "state"), AgentHome: filepath.Join(root, "home", "agent"),
		MiseBin: filepath.Join(root, "bin", "mise"),
	}
	var installs int
	marker := filepath.Join(root, "setup-runs")
	return &Applier{Cfg: cfg, InstallMise: runnertest.MiseInstaller(&installs)}, cfg, &installs, marker
}

func TestApplyIsIdempotentAndRerunsChangedSetup(t *testing.T) {
	a, cfg, installs, marker := newApplier(t)
	ctx := context.Background()
	var lines []string
	a.Progress = func(l string) { lines = append(lines, l) }
	setup := "echo run >> " + marker
	tmpl := proto.TemplateApply{Version: 1, MiseTOML: "[tools]\ngo = \"1.26\"\n", SetupScript: setup}

	log, err := a.Apply(ctx, tmpl)
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	if *installs != 1 || AppliedVersion(cfg) != 1 {
		t.Fatalf("installs=%d version=%d", *installs, AppliedVersion(cfg))
	}
	got, _ := os.ReadFile(filepath.Join(cfg.AgentHome, ".config/mise/config.toml"))
	if string(got) != tmpl.MiseTOML {
		t.Fatalf("mise config %q", got)
	}
	calls, _ := os.ReadFile(filepath.Join(cfg.AgentHome, "mise-calls.log"))
	if !strings.Contains(string(calls), "install --yes HOME="+cfg.AgentHome) {
		t.Fatalf("mise not run with the agent's HOME: %q", calls)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "mise: all tools installed") {
		t.Fatalf("progress lines missing mise output: %v", lines)
	}

	// Same template again: mise not reinstalled, setup not rerun.
	tmpl.Version = 2
	if _, err := a.Apply(ctx, tmpl); err != nil {
		t.Fatal(err)
	}
	if runs, _ := os.ReadFile(marker); strings.Count(string(runs), "run") != 1 || *installs != 1 {
		t.Fatalf("setup reran or mise reinstalled: runs=%q installs=%d", runs, *installs)
	}
	// Changed setup script: rerun.
	tmpl.Version, tmpl.SetupScript = 3, setup+" # v2"
	if _, err := a.Apply(ctx, tmpl); err != nil {
		t.Fatal(err)
	}
	if runs, _ := os.ReadFile(marker); strings.Count(string(runs), "run") != 2 || AppliedVersion(cfg) != 3 {
		t.Fatalf("changed setup not rerun: %q v%d", runs, AppliedVersion(cfg))
	}
}

func TestApplyFailuresKeepPreviousVersion(t *testing.T) {
	a, cfg, _, _ := newApplier(t)
	ctx := context.Background()
	if _, err := a.Apply(ctx, proto.TemplateApply{Version: 1, MiseTOML: "[tools]\n"}); err != nil {
		t.Fatal(err)
	}
	log, err := a.Apply(ctx, proto.TemplateApply{Version: 2, MiseTOML: "[tools]\nFAIL = \"latest\"\n"})
	if err == nil || !strings.Contains(log, "tool FAIL@latest not found") {
		t.Fatalf("want mise failure with its output, got %v\n%s", err, log)
	}
	if AppliedVersion(cfg) != 1 {
		t.Fatalf("version advanced on failure: %d", AppliedVersion(cfg))
	}
	if _, err := a.Apply(ctx, proto.TemplateApply{Version: 3, MiseTOML: "[tools]\n", SetupScript: "exit 7"}); err == nil || !strings.Contains(err.Error(), "setup script") {
		t.Fatalf("setup failure: %v", err)
	}
}

func TestApplyWithoutInstallerNeverDownloads(t *testing.T) {
	a, _, _, _ := newApplier(t)
	a.InstallMise = nil
	if _, err := a.Apply(context.Background(), proto.TemplateApply{Version: 1}); err == nil || !strings.Contains(err.Error(), "no mise installer") {
		t.Fatalf("want refusal, got %v", err)
	}
}

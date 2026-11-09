package fleet

import (
	"context"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/placement"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func TestTemplateTagsProvidesAndRunnerCopyAndProbe(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	y := "name: tagged\narch: arm64\nmin: {vcpu: 2, ram: 4GiB, disk: 20GiB}\nmise_toml: |\n  [tools]\n  go = \"1.22\"\nprovides: [net=vpn, gpu=none]\nagents: []\n"
	tpl, err := e.s.CreateTemplate(ctx, []byte(y))
	if err != nil {
		t.Fatal(err)
	}
	tags, _ := placement.TemplateTags(ctx, e.s.DB.RQ)
	if got := tags[tpl.ID].Format(); strings.Join(got, ",") != "arch=arm64,gpu=none,net=vpn,tool:go=1.22" {
		t.Fatalf("template tags %v", got)
	}
	if _, err := e.s.CreateTemplate(ctx, []byte(strings.Replace(y, "name: tagged", "name: bad", 1)+"provides: [Bad Key]\n")); err == nil {
		t.Fatal("a bad provides list was accepted")
	}
	// A runner of the template carries its template's tags; a probe adds tool:/agent: tags only.
	e.s.DB.W.Exec(`INSERT INTO runner (name, template_id, state) VALUES ('t1', ?, 'idle')`, tpl.ID)
	e.s.recordProbe(ctx, 1, proto.Facts{Tags: []string{"tool:docker", "agent:claude=authed", "arch=x86_64", "gpu=yes"}})
	if err := e.s.SyncCapabilities(ctx); err != nil {
		t.Fatal(err)
	}
	rt, _ := placement.RunnerTags(ctx, e.s.DB.RQ)
	got := strings.Join(rt[1].Format(), ",")
	if got != "agent:claude=authed,arch=arm64,gpu=none,net=vpn,tool:docker,tool:go=1.22" {
		t.Fatalf("runner tags %s", got)
	}
	// Syncing again keeps the probe tags and follows a template change.
	e.s.DB.W.Exec(`UPDATE runner_template SET arch = 'x86_64' WHERE id = ?`, tpl.ID)
	e.s.SyncCapabilities(ctx)
	rt, _ = placement.RunnerTags(ctx, e.s.DB.RQ)
	if rt[1]["arch"] != "x86_64" || rt[1]["tool:docker"] != "" {
		t.Fatalf("%v", rt[1])
	}
	if _, ok := rt[1]["agent:claude"]; !ok {
		t.Fatal("probe tag lost")
	}
}

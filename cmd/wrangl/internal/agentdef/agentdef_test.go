package agentdef

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
)

func setup(t *testing.T) (*db.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, q := range []string{
		`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES
		   ('go-claude','', '{"start":["claude","-p","{{.Prompt}}"]}', 2,4096,20),
		   ('multi','', '{}', 2,4096,20), ('bare','', '{}', 2,4096,20)`,
		`INSERT INTO project (slug, name, default_template_id) VALUES ('p','P',1)`,
	} {
		if _, err := d.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return d, ctx
}

func TestEnsureForTemplateWrapsAgentBlock(t *testing.T) {
	d, ctx := setup(t)
	tp, _ := d.RQ.GetTemplateByName(ctx, "go-claude")
	if err := EnsureForTemplate(ctx, d.Q, tp.ID, tp.Name, tp.AgentJson); err != nil {
		t.Fatal(err)
	}
	a, err := d.RQ.GetAgentByName(ctx, "claude")
	if err != nil {
		t.Fatalf("agent claude not created: %v", err)
	}
	if got, _ := d.RQ.ListTemplateAgents(ctx, tp.ID); len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("allow-list = %v", got)
	}
	// idempotent, and an identical block on another template shares the agent
	if err := EnsureForTemplate(ctx, d.Q, tp.ID, tp.Name, tp.AgentJson); err != nil {
		t.Fatal(err)
	}
	if all, _ := d.RQ.ListAgentDefs(ctx); len(all) != 1 {
		t.Fatalf("agents = %v", all)
	}
}

func TestResolve(t *testing.T) {
	d, ctx := setup(t)
	mk := func(name string) int64 {
		a, err := d.Q.InsertAgent(ctxOf(ctx), dbgen.InsertAgentParams{Name: name, SpecJson: `{"start":["` + name + `"]}`})
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	claude, codex := mk("claude"), mk("codex")
	multi, _ := d.RQ.GetTemplateByName(ctx, "multi")
	goc, _ := d.RQ.GetTemplateByName(ctx, "go-claude")
	bare, _ := d.RQ.GetTemplateByName(ctx, "bare")
	d.Q.AddTemplateAgent(ctx, dbgen.AddTemplateAgentParams{TemplateID: goc.ID, AgentID: claude})
	d.Q.AddTemplateAgent(ctx, dbgen.AddTemplateAgentParams{TemplateID: multi.ID, AgentID: claude})
	d.Q.AddTemplateAgent(ctx, dbgen.AddTemplateAgentParams{TemplateID: multi.ID, AgentID: codex})
	none, id := sql.NullInt64{}, func(i int64) sql.NullInt64 { return sql.NullInt64{Int64: i, Valid: true} }

	// the template's only agent
	if a, err := Resolve(ctx, d.RQ, goc.ID, 1, none); err != nil || a == nil || a.Name != "claude" {
		t.Fatalf("sole: %v %v", a, err)
	}
	// override not allowed: a clear refusal naming the choice, the template and what is allowed
	_, err := Resolve(ctx, d.RQ, goc.ID, 1, id(codex))
	if err == nil || !strings.Contains(err.Error(), `agent "codex" is not allowed on template "go-claude" (allowed: claude)`) {
		t.Fatalf("refusal: %v", err)
	}
	// several allowed, no default: asks for --agent; with a project default it uses that
	if _, err := Resolve(ctx, d.RQ, multi.ID, 1, none); err == nil || !strings.Contains(err.Error(), "--agent") {
		t.Fatalf("ambiguous: %v", err)
	}
	d.Q.SetProjectDefaultAgent(ctx, dbgen.SetProjectDefaultAgentParams{AgentID: sql.NullInt64{Int64: codex, Valid: true}, ID: 1})
	if a, err := Resolve(ctx, d.RQ, multi.ID, 1, none); err != nil || a.Name != "codex" {
		t.Fatalf("project default: %v %v", a, err)
	}
	if a, err := Resolve(ctx, d.RQ, multi.ID, 1, id(claude)); err != nil || a.Name != "claude" {
		t.Fatalf("override beats default: %v %v", a, err)
	}
	// a default the template does not allow falls back to the template's only agent
	if a, err := Resolve(ctx, d.RQ, goc.ID, 1, none); err != nil || a.Name != "claude" {
		t.Fatalf("default not allowed: %v %v", a, err)
	}
	// a template with no allowed agents: legacy (nil), but an explicit agent is refused
	if a, err := Resolve(ctx, d.RQ, bare.ID, 1, none); err != nil || a != nil {
		t.Fatalf("legacy: %v %v", a, err)
	}
	if _, err := Resolve(ctx, d.RQ, bare.ID, 1, id(claude)); err == nil || !strings.Contains(err.Error(), "allows no agents") {
		t.Fatalf("bare refusal: %v", err)
	}
}

func ctxOf(c context.Context) context.Context { return c }

func TestCredentialsStatusAndHints(t *testing.T) {
	agents := []dbgen.Agent{
		{Name: "claude", SpecJson: `{"start":["claude"],"secrets":{"CLAUDE_CODE_OAUTH_TOKEN":"claude_oauth_token"}}`},
		{Name: "codex", SpecJson: `{"start":["codex"],"secrets":{"CODEX_API_KEY":"codex_key"},"auth":{"secret":"codex_key","command":"codex login --api-key","note":"API billing"}}`},
		{Name: "plain", SpecJson: `{"start":["x"]}`},
	}
	got := Credentials(agents, func(n string) bool { return n == "codex_key" })
	if len(got) != 2 {
		t.Fatalf("creds: %+v", got)
	}
	c, x := got[0], got[1]
	if c.Agent != "claude" || c.Present || c.Command != "claude setup-token" {
		t.Errorf("claude: %+v", c)
	}
	if x.Agent != "codex" || !x.Present || x.Command != "codex login --api-key" || x.Note != "API billing" {
		t.Errorf("codex: %+v", x)
	}
}

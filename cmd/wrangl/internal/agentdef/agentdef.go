// Package agentdef resolves which named agent definition a run uses and keeps the template
// allow-list in step with templates written in the older style (docs/wrangl/adr/0001).
package agentdef

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// Resolve picks the agent for a run on a template: the explicit override, else the project's
// default, else the template's only allowed agent. A nil agent means the template has no allowed
// agents and still carries its own agent_json (older templates): the caller falls back to it.
// A choice the template does not allow is refused with a message naming what is allowed.
func Resolve(ctx context.Context, q *dbgen.Queries, templateID, projectID int64, override sql.NullInt64) (*dbgen.Agent, error) {
	allowed, err := q.ListTemplateAgents(ctx, templateID)
	if err != nil {
		return nil, err
	}
	tmpl, err := q.GetTemplate(ctx, templateID)
	if err != nil {
		return nil, err
	}
	if len(allowed) == 0 {
		if override.Valid {
			a, _ := q.GetAgent(ctx, override.Int64)
			return nil, fmt.Errorf("%w: template %q allows no agents, so agent %q cannot run on it", errs.Invalid, tmpl.Name, a.Name)
		}
		return nil, nil
	}
	names := make([]string, len(allowed))
	for i, a := range allowed {
		names[i] = a.Name
	}
	find := func(id int64) *dbgen.Agent {
		for i := range allowed {
			if allowed[i].ID == id {
				return &allowed[i]
			}
		}
		return nil
	}
	if override.Valid {
		if a := find(override.Int64); a != nil {
			return a, nil
		}
		want, _ := q.GetAgent(ctx, override.Int64)
		return nil, fmt.Errorf("%w: agent %q is not allowed on template %q (allowed: %s)", errs.Invalid, want.Name, tmpl.Name, strings.Join(names, ", "))
	}
	if def, err := q.GetProjectDefaultAgent(ctx, projectID); err == nil && def.Valid {
		if a := find(def.Int64); a != nil {
			return a, nil
		}
	}
	if len(allowed) == 1 {
		return &allowed[0], nil
	}
	return nil, fmt.Errorf("%w: template %q allows several agents (%s) and the project has no default among them: pass --agent NAME or set the project's default agent", errs.Invalid, tmpl.Name, strings.Join(names, ", "))
}

// Spec parses an agent definition's spec.
func Spec(a dbgen.Agent) (proto.AgentSpec, error) {
	s, err := proto.ParseAgentSpec(a.SpecJson)
	if err != nil {
		return s, fmt.Errorf("agent %s: %w", a.Name, err)
	}
	return s, nil
}

// Normalize re-encodes a spec document in canonical form (what ParseTemplate stores).
func Normalize(raw string) (string, error) {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// baseName names an agent after its binary (agy is Antigravity).
func baseName(spec proto.AgentSpec) string {
	if len(spec.Start) == 0 {
		return ""
	}
	n := path.Base(spec.Start[0])
	if n == "agy" {
		return "antigravity"
	}
	return n
}

// EnsureForTemplate keeps an older-style template (one with an embedded agent block) in step: it
// finds or creates the agent definition with that exact spec and allows it on the template. A
// template that already lists agents is left alone.
func EnsureForTemplate(ctx context.Context, q *dbgen.Queries, templateID int64, templateName, agentJSON string) error {
	spec, err := proto.ParseAgentSpec(agentJSON)
	if err != nil || len(spec.Start) == 0 {
		return nil
	}
	if cur, err := q.ListTemplateAgents(ctx, templateID); err == nil && len(cur) > 0 {
		// Re-applying a template file whose embedded block changed: if the template's only agent
		// belongs to it alone, the edit is that agent's edit.
		if len(cur) == 1 && cur[0].SpecJson != agentJSON {
			if users, err := q.ListAgentTemplates(ctx, cur[0].ID); err == nil && len(users) == 1 {
				_, err := q.UpdateAgentDef(ctx, dbgen.UpdateAgentDefParams{Description: cur[0].Description, SpecJson: agentJSON, Name: cur[0].Name})
				return err
			}
		}
		return nil
	}
	a, err := q.AgentBySpec(ctx, agentJSON)
	if errors.Is(err, sql.ErrNoRows) {
		name := baseName(spec)
		if _, e := q.GetAgentByName(ctx, name); e == nil {
			name += "-" + templateName
		}
		a, err = q.InsertAgent(ctx, dbgen.InsertAgentParams{Name: name, SpecJson: agentJSON})
	}
	if err != nil {
		return err
	}
	return q.AddTemplateAgent(ctx, dbgen.AddTemplateAgentParams{TemplateID: templateID, AgentID: a.ID})
}

// SetTemplateAgents replaces a template's allow-list with the named agents.
func SetTemplateAgents(ctx context.Context, q *dbgen.Queries, templateID int64, names []string) error {
	ids := make([]int64, 0, len(names))
	for _, n := range names {
		a, err := q.GetAgentByName(ctx, n)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: agent %q does not exist (wrangl agent create)", errs.Invalid, n)
		} else if err != nil {
			return err
		}
		ids = append(ids, a.ID)
	}
	if err := q.ClearTemplateAgents(ctx, templateID); err != nil {
		return err
	}
	for _, id := range ids {
		if err := q.AddTemplateAgent(ctx, dbgen.AddTemplateAgentParams{TemplateID: templateID, AgentID: id}); err != nil {
			return err
		}
	}
	return nil
}

// Credential is one secret an agent needs, with how to collect it and whether it is stored.
type Credential struct {
	Agent   string
	Secret  string
	Command string // how to collect it ("claude setup-token"), when known
	Note    string
	Present bool
}

// builtinAuth covers agents defined before the auth section existed.
var builtinAuth = map[string]proto.AuthSpec{
	"CLAUDE_CODE_OAUTH_TOKEN": {Command: "claude setup-token", Note: "a year-long inference token for Pro/Max/Team/Enterprise plans"},
}

// Credentials lists every secret the agents need (env secrets and secret files) and whether the
// controller has it, in agent order. has reports whether a secret is stored.
func Credentials(agents []dbgen.Agent, has func(name string) bool) []Credential {
	var out []Credential
	for _, a := range agents {
		spec, err := proto.ParseAgentSpec(a.SpecJson)
		if err != nil {
			continue
		}
		add := func(secret, builtin string) {
			c := Credential{Agent: a.Name, Secret: secret, Present: has(secret)}
			if b, ok := builtinAuth[builtin]; ok {
				c.Command, c.Note = b.Command, b.Note
			}
			if spec.Auth != nil && spec.Auth.Secret == secret {
				if spec.Auth.Command != "" {
					c.Command = spec.Auth.Command
				}
				if spec.Auth.Note != "" {
					c.Note = spec.Auth.Note
				}
			}
			out = append(out, c)
		}
		for env, secret := range spec.Secrets {
			add(secret, env)
		}
		for _, f := range spec.SecretFiles {
			add(f.Secret, "")
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

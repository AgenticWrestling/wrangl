package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/agentdef"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

var agentNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// registerAgentDefs serves the named agent definitions (not the controller's conversational
// agents under /api/v1/agents).
func registerAgentDefs(mux *http.ServeMux, d Deps) {
	db := d.Fleet.DB
	view := func(ctx context.Context, a dbgen.Agent) proto.AgentView {
		ts, _ := db.RQ.ListAgentTemplates(ctx, a.ID)
		if ts == nil {
			ts = []string{}
		}
		return proto.AgentView{Name: a.Name, Description: a.Description, Version: a.Version, Spec: json.RawMessage(a.SpecJson), Templates: ts}
	}
	// spec validates and normalises the posted spec document.
	spec := func(raw json.RawMessage) (string, error) {
		if len(raw) == 0 {
			return "", fmt.Errorf("%w: spec is required", errs.Invalid)
		}
		s, err := agentdef.Normalize(string(raw))
		if err != nil {
			return "", fmt.Errorf("%w: spec: %v", errs.Invalid, err)
		}
		as, err := proto.ParseAgentSpec(s)
		if err != nil {
			return "", fmt.Errorf("%w: spec: %v", errs.Invalid, err)
		}
		if len(as.Start) == 0 {
			return "", fmt.Errorf("%w: spec.start is required", errs.Invalid)
		}
		if err := as.Validate(); err != nil {
			return "", fmt.Errorf("%w: %v", errs.Invalid, err)
		}
		return s, nil
	}
	// byName loads the agent named in the path.
	byName := func(r *http.Request) (dbgen.Agent, error) {
		a, err := db.RQ.GetAgentByName(r.Context(), r.PathValue("name"))
		if errors.Is(err, sql.ErrNoRows) {
			return a, fmt.Errorf("%w: agent %q", errs.NotFound, r.PathValue("name"))
		}
		return a, err
	}
	mux.HandleFunc("GET /api/v1/agent-defs", getJSON(func(r *http.Request) ([]proto.AgentView, error) {
		as, err := db.RQ.ListAgentDefs(r.Context())
		if err != nil {
			return nil, err
		}
		out := []proto.AgentView{}
		for _, a := range as {
			out = append(out, view(r.Context(), a))
		}
		return out, nil
	}))
	mux.HandleFunc("GET /api/v1/agent-defs/{name}", getJSON(func(r *http.Request) (proto.AgentView, error) {
		a, err := byName(r)
		if err != nil {
			return proto.AgentView{}, err
		}
		return view(r.Context(), a), nil
	}))
	mux.HandleFunc("POST /api/v1/agent-defs", handle(http.StatusCreated, func(r *http.Request, req proto.AgentRequest) (proto.AgentView, error) {
		if !agentNameRe.MatchString(req.Name) {
			return proto.AgentView{}, fmt.Errorf("%w: agent name %q: lowercase letters, digits and '-'", errs.Invalid, req.Name)
		}
		s, err := spec(req.Spec)
		if err != nil {
			return proto.AgentView{}, err
		}
		a, err := db.Q.InsertAgent(r.Context(), dbgen.InsertAgentParams{Name: req.Name, Description: req.Description, SpecJson: s})
		if err != nil && strings.Contains(err.Error(), "UNIQUE") {
			err = fmt.Errorf("%w: agent %q exists (use edit)", errs.Conflict, req.Name)
		}
		if err != nil {
			return proto.AgentView{}, err
		}
		return view(r.Context(), a), nil
	}))
	// PUT looks the agent up before reading the body, so an unknown name is a 404 whatever the body.
	mux.HandleFunc("PUT /api/v1/agent-defs/{name}", func(w http.ResponseWriter, r *http.Request) {
		cur, err := byName(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		handle(http.StatusOK, func(r *http.Request, req proto.AgentRequest) (proto.AgentView, error) {
			s, err := spec(req.Spec)
			if err != nil {
				return proto.AgentView{}, err
			}
			desc := req.Description
			if desc == "" {
				desc = cur.Description
			}
			a, err := db.Q.UpdateAgentDef(r.Context(), dbgen.UpdateAgentDefParams{Description: desc, SpecJson: s, Name: cur.Name})
			if err != nil {
				return proto.AgentView{}, err
			}
			return view(r.Context(), a), nil
		})(w, r)
	})
	mux.HandleFunc("DELETE /api/v1/agent-defs/{name}", ack(http.StatusNoContent, func(r *http.Request) error {
		if _, err := byName(r); err != nil {
			return err
		}
		_, err := db.Q.DeleteAgentDef(r.Context(), r.PathValue("name"))
		return err
	}))
	allow := func(add bool) http.HandlerFunc {
		return ack(http.StatusNoContent, func(r *http.Request) error {
			a, err := byName(r)
			if err != nil {
				return err
			}
			t, err := d.Fleet.Template(r.Context(), r.PathValue("template"))
			if err != nil {
				return err
			}
			if add {
				return db.Q.AddTemplateAgent(r.Context(), dbgen.AddTemplateAgentParams{TemplateID: t.ID, AgentID: a.ID})
			}
			return db.Q.RemoveTemplateAgent(r.Context(), dbgen.RemoveTemplateAgentParams{TemplateID: t.ID, AgentID: a.ID})
		})
	}
	mux.HandleFunc("PUT /api/v1/agent-defs/{name}/templates/{template}", allow(true))
	mux.HandleFunc("DELETE /api/v1/agent-defs/{name}/templates/{template}", allow(false))
}

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
	get := func(w http.ResponseWriter, r *http.Request) (dbgen.Agent, bool) {
		a, err := db.RQ.GetAgentByName(r.Context(), r.PathValue("name"))
		if errors.Is(err, sql.ErrNoRows) {
			writeErr(w, fmt.Errorf("%w: agent %q", errs.NotFound, r.PathValue("name")))
			return a, false
		} else if err != nil {
			writeErr(w, err)
			return a, false
		}
		return a, true
	}
	mux.HandleFunc("GET /api/v1/agent-defs", func(w http.ResponseWriter, r *http.Request) {
		as, err := db.RQ.ListAgentDefs(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		out := []proto.AgentView{}
		for _, a := range as {
			out = append(out, view(r.Context(), a))
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/v1/agent-defs/{name}", func(w http.ResponseWriter, r *http.Request) {
		if a, ok := get(w, r); ok {
			writeJSON(w, http.StatusOK, view(r.Context(), a))
		}
	})
	mux.HandleFunc("POST /api/v1/agent-defs", func(w http.ResponseWriter, r *http.Request) {
		var req proto.AgentRequest
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		if !agentNameRe.MatchString(req.Name) {
			writeErr(w, fmt.Errorf("%w: agent name %q: lowercase letters, digits and '-'", errs.Invalid, req.Name))
			return
		}
		s, err := spec(req.Spec)
		if err != nil {
			writeErr(w, err)
			return
		}
		a, err := db.Q.InsertAgent(r.Context(), dbgen.InsertAgentParams{Name: req.Name, Description: req.Description, SpecJson: s})
		if err != nil && strings.Contains(err.Error(), "UNIQUE") {
			err = fmt.Errorf("%w: agent %q exists (use edit)", errs.Conflict, req.Name)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, view(r.Context(), a))
	})
	mux.HandleFunc("PUT /api/v1/agent-defs/{name}", func(w http.ResponseWriter, r *http.Request) {
		cur, ok := get(w, r)
		if !ok {
			return
		}
		var req proto.AgentRequest
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		s, err := spec(req.Spec)
		if err != nil {
			writeErr(w, err)
			return
		}
		desc := req.Description
		if desc == "" {
			desc = cur.Description
		}
		a, err := db.Q.UpdateAgentDef(r.Context(), dbgen.UpdateAgentDefParams{Description: desc, SpecJson: s, Name: cur.Name})
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view(r.Context(), a))
	})
	mux.HandleFunc("DELETE /api/v1/agent-defs/{name}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := get(w, r); !ok {
			return
		}
		if _, err := db.Q.DeleteAgentDef(r.Context(), r.PathValue("name")); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	allow := func(add bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			a, ok := get(w, r)
			if !ok {
				return
			}
			t, err := d.Fleet.Template(r.Context(), r.PathValue("template"))
			if err != nil {
				writeErr(w, err)
				return
			}
			if add {
				err = db.Q.AddTemplateAgent(r.Context(), dbgen.AddTemplateAgentParams{TemplateID: t.ID, AgentID: a.ID})
			} else {
				err = db.Q.RemoveTemplateAgent(r.Context(), dbgen.RemoveTemplateAgentParams{TemplateID: t.ID, AgentID: a.ID})
			}
			if err != nil {
				writeErr(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}
	mux.HandleFunc("PUT /api/v1/agent-defs/{name}/templates/{template}", allow(true))
	mux.HandleFunc("DELETE /api/v1/agent-defs/{name}/templates/{template}", allow(false))
}

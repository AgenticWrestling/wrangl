package api

import (
	"encoding/json"
	"net/http"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/tools"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// registerTools serves the custom tool library and its per-project enablement (ADR 0003).
func registerTools(mux *http.ServeMux, d Deps) {
	svc := d.Tools
	if svc == nil {
		svc = &tools.Service{DB: d.Fleet.DB}
	}
	view := func(t tools.Tool, full bool) proto.LibraryToolView {
		v := proto.LibraryToolView{Name: t.Name, Kind: t.Kind, Effect: t.Effect, Description: t.Description, Version: t.Version, Projects: t.Projects}
		var decl struct {
			RunKinds []string `json:"run_kinds"`
		}
		json.Unmarshal([]byte(t.DeclJson), &decl)
		v.RunKinds = decl.RunKinds
		if v.Projects == nil {
			v.Projects = []string{}
		}
		if full {
			v.Declaration = json.RawMessage(t.DeclJson)
			v.KDL = t.Kdl
		}
		return v
	}
	views := func(ts []tools.Tool, err error) ([]proto.LibraryToolView, error) {
		out := []proto.LibraryToolView{}
		for _, t := range ts {
			out = append(out, view(t, false))
		}
		return out, err
	}
	mux.HandleFunc("GET /api/v1/tools", getJSON(func(r *http.Request) ([]proto.LibraryToolView, error) {
		return views(svc.List(r.Context()))
	}))
	mux.HandleFunc("GET /api/v1/tools/{name}", getJSON(func(r *http.Request) (proto.LibraryToolView, error) {
		t, err := svc.Get(r.Context(), r.PathValue("name"))
		return view(t, true), err
	}))
	mux.HandleFunc("POST /api/v1/tools", handle(http.StatusCreated, func(r *http.Request, req proto.LibraryToolRequest) (proto.LibraryToolView, error) {
		t, err := svc.Add(r.Context(), req.KDL)
		return view(t, true), err
	}))
	// PUT looks the tool up before reading the body, so an unknown name is a 404 whatever the body.
	mux.HandleFunc("PUT /api/v1/tools/{name}", func(w http.ResponseWriter, r *http.Request) {
		if _, err := svc.Get(r.Context(), r.PathValue("name")); err != nil {
			writeErr(w, err)
			return
		}
		handle(http.StatusOK, func(r *http.Request, req proto.LibraryToolRequest) (proto.LibraryToolView, error) {
			t, err := svc.Update(r.Context(), r.PathValue("name"), req.KDL)
			return view(t, true), err
		})(w, r)
	})
	mux.HandleFunc("DELETE /api/v1/tools/{name}", ack(http.StatusNoContent, func(r *http.Request) error {
		return svc.Remove(r.Context(), r.PathValue("name"), r.URL.Query().Get("force") == "1")
	}))
	mux.HandleFunc("GET /api/v1/projects/{slug}/tools", getJSON(func(r *http.Request) ([]proto.LibraryToolView, error) {
		return views(svc.ForProject(r.Context(), r.PathValue("slug")))
	}))
	mux.HandleFunc("PUT /api/v1/projects/{slug}/tools/{name}", ack(http.StatusNoContent, func(r *http.Request) error {
		return svc.Enable(r.Context(), r.PathValue("slug"), r.PathValue("name"))
	}))
	mux.HandleFunc("DELETE /api/v1/projects/{slug}/tools/{name}", ack(http.StatusNoContent, func(r *http.Request) error {
		return svc.Disable(r.Context(), r.PathValue("slug"), r.PathValue("name"))
	}))
	// Executing tools arrives with the proxy (ADR 0003 phase 1d); until then the endpoint checks
	// the tool is enabled and says so.
	mux.HandleFunc("POST /api/v1/projects/{slug}/tools/{name}/test", func(w http.ResponseWriter, r *http.Request) {
		if _, err := svc.Enabled(r.Context(), r.PathValue("slug"), r.PathValue("name")); err != nil {
			writeErr(w, err)
			return
		}
		var req proto.LibraryToolTestRequest
		if r.ContentLength != 0 {
			if err := decode(r, &req); err != nil {
				writeErr(w, err)
				return
			}
		}
		writeJSON(w, http.StatusNotImplemented, proto.APIError{Error: "tool execution is not implemented yet (ADR 0003 phase 1d); the tool is enabled and its definition is valid"})
	})
}

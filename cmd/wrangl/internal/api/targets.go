package api

import (
	"net/http"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// registerTargets serves remote targets (docs/wrangl/adr/0002).
func registerTargets(mux *http.ServeMux, d Deps) {
	db := d.Fleet.DB
	mux.HandleFunc("GET /api/v1/targets", func(w http.ResponseWriter, r *http.Request) {
		ts, err := db.RQ.ListRemoteTargets(r.Context(), r.URL.Query().Get("all") == "1")
		if err != nil {
			writeErr(w, err)
			return
		}
		out := []proto.TargetView{}
		for _, g := range ts {
			v := proto.TargetView{Name: g.Name, State: g.State, InstanceID: g.InstanceID.String, IdleSince: g.IdleSince.String, LastError: g.LastError}
			if t, err := db.RQ.GetTemplate(r.Context(), g.TemplateID); err == nil {
				v.Template, v.MaxSessions = t.Name, t.MaxSessions
			}
			v.Sessions, _ = db.RQ.CountTargetSessions(r.Context(), g.ID)
			if g.LaunchedForProject.Valid {
				if p, err := db.RQ.GetProject(r.Context(), g.LaunchedForProject.Int64); err == nil {
					v.Project = p.Slug
				}
			}
			out = append(out, v)
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("DELETE /api/v1/targets/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Fleet.TerminateTarget(r.Context(), r.PathValue("name")); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

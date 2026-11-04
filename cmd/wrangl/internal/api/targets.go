package api

import (
	"net/http"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// registerTargets serves remote targets (docs/wrangl/adr/0002).
func registerTargets(mux *http.ServeMux, d Deps) {
	db := d.Fleet.DB
	mux.HandleFunc("GET /api/v1/targets", getJSON(func(r *http.Request) ([]proto.TargetView, error) {
		ts, err := db.RQ.ListRemoteTargets(r.Context(), r.URL.Query().Get("all") == "1")
		if err != nil {
			return nil, err
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
		return out, nil
	}))
	mux.HandleFunc("DELETE /api/v1/targets/{name}", ack(http.StatusNoContent, func(r *http.Request) error {
		return d.Fleet.TerminateTarget(r.Context(), r.PathValue("name"))
	}))
}

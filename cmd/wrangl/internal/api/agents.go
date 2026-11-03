package api

import (
	"net/http"

	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbgen"
)

func registerAgents(mux *http.ServeMux, d Deps) {
	if d.Agents == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/agents", func(w http.ResponseWriter, r *http.Request) {
		rows, err := d.Agents.DB.RQ.ListControllerAgents(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	})
	mux.HandleFunc("PUT /api/v1/agents/{name}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			dbgen.ControllerAgent
			Default bool `json:"default"`
		}
		if err := decode(r, &in); err != nil {
			writeErr(w, err)
			return
		}
		in.Name = r.PathValue("name")
		if err := d.Agents.Save(r.Context(), in.ControllerAgent, in.Default); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /api/v1/agents/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := d.Agents.Delete(r.Context(), r.PathValue("name")); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/tasks/{sqid}/ask", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Agent    string `json:"agent"`
			Question string `json:"question"`
		}
		if err := decode(r, &in); err != nil {
			writeErr(w, err)
			return
		}
		m, err := d.Agents.Ask(r.Context(), r.PathValue("sqid"), in.Agent, in.Question)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, m)
	})
	mux.HandleFunc("GET /api/v1/tasks/{sqid}/thread", func(w http.ResponseWriter, r *http.Request) {
		t, err := d.Stack.Task(r.Context(), d.Agents.DB.RQ, r.PathValue("sqid"))
		if err != nil {
			writeErr(w, err)
			return
		}
		rows, err := d.Agents.DB.RQ.ListThread(r.Context(), t.ID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	})
}

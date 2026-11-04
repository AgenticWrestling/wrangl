package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

func registerAgents(mux *http.ServeMux, d Deps) {
	if d.Agents == nil {
		return
	}
	rq := d.Agents.DB.RQ
	mux.HandleFunc("GET /api/v1/agents", getJSON(func(r *http.Request) ([]dbgen.ControllerAgent, error) {
		return rq.ListControllerAgents(r.Context())
	}))
	type saveRequest struct {
		dbgen.ControllerAgent
		Default bool `json:"default"`
	}
	mux.HandleFunc("PUT /api/v1/agents/{name}", handle(http.StatusNoContent, func(r *http.Request, in saveRequest) (none, error) {
		in.Name = r.PathValue("name")
		return none{}, d.Agents.Save(r.Context(), in.ControllerAgent, in.Default)
	}))
	mux.HandleFunc("DELETE /api/v1/agents/{name}", ack(http.StatusNoContent, func(r *http.Request) error {
		return d.Agents.Delete(r.Context(), r.PathValue("name"))
	}))
	type askRequest struct {
		Agent    string `json:"agent"`
		Question string `json:"question"`
	}
	mux.HandleFunc("POST /api/v1/tasks/{sqid}/ask", handle(http.StatusAccepted, func(r *http.Request, in askRequest) (dbgen.TaskThread, error) {
		return d.Agents.Ask(r.Context(), r.PathValue("sqid"), in.Agent, in.Question)
	}))
	mux.HandleFunc("POST /api/v1/thread/{id}/apply/{n}", ack(http.StatusNoContent, func(r *http.Request) error {
		id, e1 := strconv.ParseInt(r.PathValue("id"), 10, 64)
		n, e2 := strconv.Atoi(r.PathValue("n"))
		if e1 != nil || e2 != nil {
			return fmt.Errorf("%w: bad id", errs.Invalid)
		}
		return d.Agents.Apply(r.Context(), id, n)
	}))
	mux.HandleFunc("GET /api/v1/tasks/{sqid}/thread", getJSON(func(r *http.Request) ([]dbgen.TaskThread, error) {
		t, err := d.Stack.Task(r.Context(), rq, r.PathValue("sqid"))
		if err != nil {
			return nil, err
		}
		return rq.ListThread(r.Context(), t.ID)
	}))
}

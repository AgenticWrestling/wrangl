// Package api serves the controller's HTTP JSON API (/api/v1) and the runner enrolment
// endpoint. The CLI, TUI and (later) the PWA all use these endpoints.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/nreid/devhost/cmd/wrangl/internal/controller/fleet"
	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbgen"
	"github.com/nreid/devhost/cmd/wrangl/internal/proto"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, fleet.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, fleet.ErrInvalid):
		code = http.StatusBadRequest
	case errors.Is(err, fleet.ErrForbidden):
		code = http.StatusForbidden
	case errors.Is(err, fleet.ErrConflict):
		code = http.StatusConflict
	case errors.Is(err, fleet.ErrNoAWS):
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, proto.APIError{Error: err.Error()})
}

func decode(r *http.Request, v any) error {
	d := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", fleet.ErrInvalid, err)
	}
	return nil
}

// Register mounts /api/v1 on mux.
func Register(mux *http.ServeMux, f *fleet.Service) {
	mux.HandleFunc("GET /api/v1/runners", func(w http.ResponseWriter, r *http.Request) {
		all := int64(0)
		if r.URL.Query().Get("all") == "1" {
			all = 1
		}
		rows, err := f.DB.RQ.ListRunners(r.Context(), all)
		if err != nil {
			writeErr(w, err)
			return
		}
		out := []proto.RunnerView{}
		for _, row := range rows {
			out = append(out, runnerView(row.Runner, row.TemplateName, 0))
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/v1/runners/{name}", func(w http.ResponseWriter, r *http.Request) {
		row, err := f.DB.RQ.GetRunnerByName(r.Context(), r.PathValue("name"))
		if err != nil {
			writeErr(w, notFound(err, "runner "+r.PathValue("name")))
			return
		}
		t, err := f.DB.RQ.GetTemplate(r.Context(), row.TemplateID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, runnerView(row, t.Name, t.IdleTimeoutS))
	})
	mux.HandleFunc("POST /api/v1/runners", func(w http.ResponseWriter, r *http.Request) {
		var req proto.LaunchRequest
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		if req.Count == 0 {
			req.Count = 1
		}
		if req.Count < 0 || req.Count > 20 {
			writeErr(w, fmt.Errorf("%w: count must be 1..20", fleet.ErrInvalid))
			return
		}
		out := []proto.RunnerView{}
		for i := 0; i < req.Count; i++ {
			row, err := f.Launch(r.Context(), req.Template)
			if err != nil {
				if len(out) > 0 {
					err = fmt.Errorf("launched %d of %d, then: %w", len(out), req.Count, err)
				}
				writeErr(w, err)
				return
			}
			out = append(out, runnerView(row, req.Template, 0))
		}
		writeJSON(w, http.StatusCreated, out)
	})
	mux.HandleFunc("DELETE /api/v1/runners/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := f.Terminate(r.Context(), r.PathValue("name")); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/enroll-tokens", func(w http.ResponseWriter, r *http.Request) {
		var req proto.EnrollTokenRequest
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		ttl := time.Duration(req.TTLS) * time.Second
		if ttl == 0 {
			ttl = fleet.TokenTTL
		}
		tok, exp, err := f.MintToken(r.Context(), req.Template, ttl)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, proto.EnrollTokenResponse{
			Token: tok, ExpiresAt: exp, EnrollURL: f.EnrollURL,
			Command: fmt.Sprintf("sudo wrangl runner bootstrap --controller %s --token %s", f.EnrollURL, tok),
		})
	})
	mux.HandleFunc("GET /api/v1/templates", func(w http.ResponseWriter, r *http.Request) {
		rows, err := f.DB.RQ.ListTemplates(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		out := []proto.TemplateView{}
		for _, t := range rows {
			out = append(out, templateView(t))
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/v1/templates/{name}", func(w http.ResponseWriter, r *http.Request) {
		t, err := f.Template(r.Context(), r.PathValue("name"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, templateView(t))
	})
	mux.HandleFunc("POST /api/v1/templates", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErr(w, err)
			return
		}
		t, err := f.CreateTemplate(r.Context(), body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, templateView(t))
	})
	mux.HandleFunc("DELETE /api/v1/templates/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := f.DeleteTemplate(r.Context(), r.PathValue("name")); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func notFound(err error, what string) error {
	if err != nil && err.Error() == "sql: no rows in result set" {
		return fmt.Errorf("%w: %s", fleet.ErrNotFound, what)
	}
	return err
}

func runnerView(r dbgen.Runner, template string, idleTimeout int64) proto.RunnerView {
	v := proto.RunnerView{
		ID: r.ID, Name: r.Name, State: r.State, Template: template, Launched: r.Launched == 1,
		InstanceID: r.InstanceID.String, InstanceType: r.InstanceType, AZ: r.Az, PrivateIP: r.PrivateIp,
		Arch: r.Arch, VCPU: r.Vcpu, RAMMiB: r.RamMib, DiskGiB: r.DiskGib,
		LastHeartbeatAt: r.LastHeartbeatAt.String, IdleSince: r.IdleSince.String, IdleTimeoutS: idleTimeout,
		LaunchedAt: r.LaunchedAt.String, EnrolledAt: r.EnrolledAt.String, TerminatedAt: r.TerminatedAt.String,
	}
	if r.LastMetricsJson != "" && r.LastMetricsJson != "{}" {
		v.Metrics = json.RawMessage(r.LastMetricsJson)
	}
	return v
}

func templateView(t dbgen.RunnerTemplate) proto.TemplateView {
	v := proto.TemplateView{
		Name: t.Name, Description: t.Description, Arch: t.Arch, MinVCPU: t.MinVcpu, MinRAMMiB: t.MinRamMib,
		MinDiskGiB: t.MinDiskGib, AMI: t.Ami, MaxRunners: t.MaxRunners, IdleTimeoutS: t.IdleTimeoutS,
		Version: t.Version, MiseTOML: t.MiseToml, SetupScript: t.SetupScript, Agent: json.RawMessage(t.AgentJson),
	}
	json.Unmarshal([]byte(t.InstanceTypes), &v.InstanceTypes)
	if v.InstanceTypes == nil {
		v.InstanceTypes = []string{}
	}
	return v
}

// EnrollHandler serves the runner-facing listener (:4223): enrolment and binary
// downloads. It is reachable from the VPC only and exposes nothing else.
func EnrollHandler(f *fleet.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+proto.EnrollPath, func(w http.ResponseWriter, r *http.Request) {
		var req proto.EnrollRequest
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			writeErr(w, err)
			return
		}
		resp, err := f.Enroll(r.Context(), req, host)
		if err != nil {
			f.Log.Warn("enrol rejected", "from", host, "err", err)
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
	mux.HandleFunc("GET /dl/{file}", func(w http.ResponseWriter, r *http.Request) {
		var arch string
		if _, err := fmt.Sscanf(r.PathValue("file"), "wrangl-linux-%s", &arch); err != nil || (arch != "aarch64" && arch != "x86_64") {
			http.NotFound(w, r)
			return
		}
		path, ok := f.BinaryPath(arch)
		if !ok {
			http.Error(w, "no runner binary for "+arch, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, path)
	})
	return mux
}

// Package api serves the controller's HTTP JSON API (/api/v1) and the runner enrolment
// endpoint. The CLI, TUI and (later) the PWA all use these endpoints.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/menu"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/placement"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/agents"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/costs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/deliverables"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/fleet"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/ghsetup"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/plans"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/projects"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/provision"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/prs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/webpush"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/events"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, projects.ErrNoGitHub):
		code = http.StatusServiceUnavailable
	case errors.Is(err, errs.NotFound):
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

// Deps are the services the API exposes.
type Deps struct {
	Fleet    *fleet.Service
	Stack    *stack.Service
	Projects *projects.Service
	Runs     *runs.Service
	PRs      *prs.Service
	Plans    *plans.Service
	Push     *webpush.Service
	JS       jetstream.JetStream
	Bus      *events.Bus
	Setup    *ghsetup.Service
	// Secrets is the controller's secret store (nil: secrets cannot be managed over the API).
	Secrets *SecretStore
	// Costs reports spend and the account's other instances (nil = no AWS access).
	Costs *costs.Service
	// Deliverables stores mockups, prototype links and reports handed back by runs.
	Deliverables *deliverables.Service
	Term         *TermHub
	// Agents runs controller agents ("Ask?" on a task).
	Agents *agents.Service
}

// Register mounts /api/v1 on mux.
func Register(mux *http.ServeMux, d Deps) {
	f := d.Fleet
	registerWork(mux, d)
	registerTerm(mux, d)
	registerAgentDefs(mux, d)
	registerSecrets(mux, d)
	registerTargets(mux, d)
	registerCosts(mux, d)
	registerAgents(mux, d)
	mux.HandleFunc("GET /api/v1/events", func(w http.ResponseWriter, r *http.Request) { streamEvents(w, r, d.Bus) })
	mux.HandleFunc("GET /api/v1/push/key", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"public_key": d.Push.Keys.Public})
	})
	mux.HandleFunc("POST /api/v1/push/subscribe", func(w http.ResponseWriter, r *http.Request) {
		var sub struct {
			Endpoint string `json:"endpoint"`
			Keys     struct {
				P256dh string `json:"p256dh"`
				Auth   string `json:"auth"`
			} `json:"keys"`
			ExpirationTime any `json:"expirationTime"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&sub); err != nil {
			writeErr(w, fmt.Errorf("%w: %v", errs.Invalid, err))
			return
		}
		if err := d.Push.Subscribe(r.Context(), sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth, r.UserAgent()); err != nil {
			writeErr(w, fmt.Errorf("%w: %v", errs.Invalid, err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/push/test", func(w http.ResponseWriter, r *http.Request) {
		n, err := d.Push.Send(r.Context(), webpush.Notification{Title: "wrangl", Body: "Test notification", URL: "/", Tag: "test"})
		if err != nil && n == 0 {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"delivered": n})
	})
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
			v := runnerView(row.Runner, row.TemplateName, 0)
			v.Capacity = capacityView(r.Context(), f.DB, row.Runner)
			out = append(out, v)
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
		v := runnerView(row, t.Name, t.IdleTimeoutS)
		v.WantVersion = t.Version
		v.TemplateLog = row.TemplateLog
		v.Capacity = capacityView(r.Context(), f.DB, row)
		writeJSON(w, http.StatusOK, v)
	})
	mux.HandleFunc("PATCH /api/v1/runners/{name}", handle(http.StatusOK, func(r *http.Request, req proto.RunnerPatch) (proto.RunnerView, error) {
		row, err := f.DB.RQ.GetRunnerByName(r.Context(), r.PathValue("name"))
		if err != nil {
			return proto.RunnerView{}, notFound(err, "runner "+r.PathValue("name"))
		}
		if len(req.Capacity) > 0 {
			if err := (&placement.Service{DB: f.DB}).SetRunnerCapacity(r.Context(), row.ID, req.Capacity); err != nil {
				return proto.RunnerView{}, err
			}
		}
		v := runnerView(row, "", 0)
		v.Capacity = capacityView(r.Context(), f.DB, row)
		return v, nil
	}))
	for _, action := range []string{"cordon", "uncordon", "drain"} {
		mux.HandleFunc("POST /api/v1/runners/{name}/"+action, func(w http.ResponseWriter, r *http.Request) {
			name := r.PathValue("name")
			var err error
			switch action {
			case "cordon":
				err = f.Cordon(r.Context(), name, true)
			case "uncordon":
				err = f.Cordon(r.Context(), name, false)
			case "drain":
				err = f.Drain(r.Context(), name)
			}
			if err != nil {
				writeErr(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}
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
	mux.HandleFunc("PUT /api/v1/templates/{name}", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErr(w, err)
			return
		}
		t, err := f.UpdateTemplate(r.Context(), r.PathValue("name"), body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, templateView(t))
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
		Cordoned: r.Cordoned == 1, Draining: r.DrainRequested == 1, TemplateVersion: r.TemplateVersion,
		LastError: r.LastError, BinarySHA: r.BinarySha, UpgradeSHA: r.UpgradeSha,
	}
	if r.LastMetricsJson != "" && r.LastMetricsJson != "{}" {
		v.Metrics = json.RawMessage(r.LastMetricsJson)
	}
	return v
}

func templateView(t dbgen.RunnerTemplate) proto.TemplateView {
	v := proto.TemplateView{
		Name: t.Name, Description: t.Description, Arch: t.Arch, MinVCPU: t.MinVcpu, MinRAMMiB: t.MinRamMib,
		MinDiskGiB: t.MinDiskGib, AMI: t.Ami, MaxRunners: t.MaxRunners, MaxSessions: t.MaxSessions, IdleTimeoutS: t.IdleTimeoutS,
		Version: t.Version, FailedVersion: t.FailedVersion, MiseTOML: t.MiseToml, SetupScript: t.SetupScript, Agent: json.RawMessage(t.AgentJson),
	}
	v.InstanceTypes = menu.Names(t.InstanceTypes)
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
	// A test instance (Settings > Runner templates > Test this image) reports what it found.
	mux.HandleFunc("POST /imagecheck/{token}", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
		rep, perr := provision.ParseProbeReport(b)
		if err != nil || perr != nil || !f.SubmitProbe(r.PathValue("token"), rep) {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
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

// KeepAlive is how often an idle SSE stream sends a comment (Cloudflare drops idle
// connections after ~100 s).
var KeepAlive = 15 * time.Second

// sseStart sets SSE headers and returns the flusher.
func sseStart(w http.ResponseWriter) http.Flusher {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	fmt.Fprint(w, ": connected\n\n")
	if fl != nil {
		fl.Flush()
	}
	return fl
}

// streamEvents is GET /api/v1/events: change notices as SSE until the client leaves.
func streamEvents(w http.ResponseWriter, r *http.Request, bus *events.Bus) {
	if bus == nil {
		writeErr(w, fmt.Errorf("event stream unavailable"))
		return
	}
	ch, stop := bus.Subscribe()
	defer stop()
	fl := sseStart(w)
	tick := time.NewTicker(KeepAlive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Kind, b)
		case <-tick.C:
			fmt.Fprint(w, ": ping\n\n")
		}
		if fl != nil {
			fl.Flush()
		}
	}
}

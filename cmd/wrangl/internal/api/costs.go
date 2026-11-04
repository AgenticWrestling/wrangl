package api

import (
	"context"
	"fmt"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/costs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// OtherInstances are the account's instances that are not wrangl runners (a runner is known by
// its instance id), cheapest last, as of the cached snapshot.
func OtherInstances(snap costs.Snapshot, runnerInstances map[string]bool) []costs.Instance {
	var out []costs.Instance
	for _, in := range snap.Instances {
		if runnerInstances[in.ID] {
			continue
		}
		out = append(out, in)
	}
	return out
}

// AdoptInfo says whether an instance can reach the controller's enrol listener.
func AdoptInfo(snap costs.Snapshot, enrolURL string, in costs.Instance) (reachable bool, note string) {
	u, err := url.Parse(enrolURL)
	if err != nil || u.Hostname() == "" {
		return false, "the controller's enrol URL is not set"
	}
	for _, c := range snap.Instances { // the controller's own instance, found by its private address
		if c.PrivateIP == u.Hostname() {
			if c.Region == in.Region && c.VPC == in.VPC && in.VPC != "" {
				return true, ""
			}
			return false, fmt.Sprintf("%s is in %s/%s; this instance is in %s/%s. It needs a route to %s (VPC peering or a transit gateway) before it can enrol.",
				enrolURL, c.Region, c.VPC, in.Region, in.VPC, u.Host)
		}
	}
	return false, "could not tell which network the controller is in; make sure the instance can reach " + u.Host
}

func registerCosts(mux *http.ServeMux, d Deps) {
	if d.Costs == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/costs", func(w http.ResponseWriter, r *http.Request) {
		s := d.Costs.Cached()
		writeJSON(w, http.StatusOK, map[string]any{"as_of": s.AsOf, "summary": s.Summary, "errors": s.Errors})
	})
	mux.HandleFunc("GET /api/v1/instances", func(w http.ResponseWriter, r *http.Request) {
		known, err := runnerInstanceIDs(r, d)
		if err != nil {
			writeErr(w, err)
			return
		}
		s := d.Costs.Cached()
		writeJSON(w, http.StatusOK, map[string]any{"as_of": s.AsOf, "instances": OtherInstances(s, known), "errors": s.Errors})
	})
	mux.HandleFunc("GET /api/v1/instance-types", func(w http.ResponseWriter, r *http.Request) {
		cat, err := d.Costs.Catalogue(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		if arch := r.URL.Query().Get("arch"); arch != "" {
			var f []costs.CatalogueType
			for _, c := range cat {
				if c.Arch == arch {
					f = append(f, c)
				}
			}
			cat = f
		}
		writeJSON(w, http.StatusOK, map[string]any{"types": cat})
	})
	mux.HandleFunc("GET /api/v1/images", func(w http.ResponseWriter, r *http.Request) {
		if d.Fleet.Prov == nil {
			writeErr(w, fmt.Errorf("%w: AWS is not configured on this controller", errs.Invalid))
			return
		}
		imgs, err := d.Fleet.Prov.SearchMarketplace(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("arch"))
		if err != nil {
			writeErr(w, fmt.Errorf("%w: %v", errs.Invalid, err))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"images": imgs})
	})
	mux.HandleFunc("GET /api/v1/images/check", func(w http.ResponseWriter, r *http.Request) {
		if d.Fleet.Prov == nil {
			writeErr(w, fmt.Errorf("%w: AWS is not configured on this controller", errs.Invalid))
			return
		}
		q := r.URL.Query()
		if !strings.HasPrefix(q.Get("ami"), "ami-") || q.Get("type") == "" {
			writeErr(w, fmt.Errorf("%w: ami and type are required", errs.Invalid))
			return
		}
		writeJSON(w, http.StatusOK, d.Fleet.Prov.CheckLaunch(r.Context(), q.Get("ami"), q.Get("type"), d.Fleet.SubnetID, d.Fleet.SecurityGroupID))
	})
	// The full test: launches one instance (about two minutes, a few cents) and terminates it.
	mux.HandleFunc("POST /api/v1/images/test", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if !strings.HasPrefix(q.Get("ami"), "ami-") || q.Get("type") == "" || (q.Get("arch") != "arm64" && q.Get("arch") != "x86_64") {
			writeErr(w, fmt.Errorf("%w: ami, type and arch (arm64|x86_64) are required", errs.Invalid))
			return
		}
		res, err := d.Fleet.TestImage(r.Context(), q.Get("ami"), q.Get("type"), q.Get("arch"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("DELETE /api/v1/instances/{region}/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, err := TerminateOther(r.Context(), d, r.PathValue("region"), r.PathValue("id")); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/instances/{region}/{id}/adopt", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Template string `json:"template"`
		}
		if err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		res, err := Adopt(r.Context(), d, r.PathValue("region"), r.PathValue("id"), req.Template)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, res)
	})
}

// TerminateOther terminates an instance listed under "Other instances". It refuses anything
// that is not in the last scan, that is a live runner (use the runner's own Terminate), or
// that is the controller itself.
func TerminateOther(ctx context.Context, d Deps, region, id string) (name string, err error) {
	snap := d.Costs.Cached()
	var in *costs.Instance
	for i := range snap.Instances {
		if snap.Instances[i].ID == id && snap.Instances[i].Region == region {
			in = &snap.Instances[i]
		}
	}
	if in == nil {
		return "", fmt.Errorf("%w: no instance %s in %s", errs.NotFound, id, region)
	}
	if in.State != "running" && in.State != "stopped" && in.State != "pending" && in.State != "stopping" {
		return "", fmt.Errorf("%w: %s is %s", errs.Conflict, id, in.State)
	}
	if u, perr := url.Parse(d.Fleet.EnrollURL); perr == nil && u.Hostname() != "" && in.PrivateIP == u.Hostname() {
		return "", fmt.Errorf("%w: %s is the controller's own host", errs.Forbidden, id)
	}
	rows, err := d.Fleet.DB.RQ.ListRunners(ctx, 0)
	if err != nil {
		return "", err
	}
	for _, x := range rows {
		if x.Runner.InstanceID.String == id && x.Runner.State != "terminated" {
			return "", fmt.Errorf("%w: %s is runner %s; terminate it from Runners", errs.Conflict, id, x.Runner.Name)
		}
	}
	name = in.Name
	if name == "" {
		name = id
	}
	if err := d.Costs.Terminate(ctx, region, id); err != nil {
		return "", err
	}
	_ = d.Fleet.DB.Q.AddLogEntry(context.WithoutCancel(ctx), dbgen.AddLogEntryParams{
		At: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Kind: "runner", Level: "warn", Subject: name,
		Title: "Instance terminated", Detail: fmt.Sprintf("%s in %s, %s (not a runner), from the web UI", id, region, in.Type),
	})
	return name, nil
}

// AdoptResult is what an operator needs to turn a running instance into a runner.
type AdoptResult struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Reachable bool      `json:"reachable"`
	Note      string    `json:"note,omitempty"`
	Command   string    `json:"command"`
	Binary    string    `json:"binary"`
}

// Adopt mints an enrolment token for a running instance that is not a runner and returns the
// command to run on it. The instance must be able to reach the controller's enrol listener;
// Reachable and Note say whether it can.
func Adopt(ctx context.Context, d Deps, region, id, template string) (*AdoptResult, error) {
	snap := d.Costs.Cached()
	var in *costs.Instance
	for i := range snap.Instances {
		if snap.Instances[i].ID == id && snap.Instances[i].Region == region {
			in = &snap.Instances[i]
		}
	}
	if in == nil || in.State != "running" {
		return nil, fmt.Errorf("%w: no running instance %s in %s", errs.NotFound, id, region)
	}
	rows, err := d.Fleet.DB.RQ.ListRunners(ctx, 0)
	if err != nil {
		return nil, err
	}
	for _, x := range rows {
		if x.Runner.InstanceID.String == id && x.Runner.State != "terminated" {
			return nil, fmt.Errorf("%w: %s is already a runner", errs.Conflict, id)
		}
	}
	t, err := d.Fleet.Template(ctx, template)
	if err != nil {
		return nil, err
	}
	if in.Arch != "" && in.Arch != t.Arch {
		return nil, fmt.Errorf("%w: %s is %s but template %s is %s", errs.Invalid, id, in.Arch, t.Name, t.Arch)
	}
	tok, exp, err := d.Fleet.MintToken(ctx, template, 30*time.Minute)
	if err != nil {
		return nil, err
	}
	reach, note := AdoptInfo(snap, d.Fleet.EnrollURL, *in)
	uname := map[string]string{"arm64": "aarch64", "x86_64": "x86_64"}[t.Arch]
	return &AdoptResult{Token: tok, ExpiresAt: exp, Reachable: reach, Note: note,
		Command: fmt.Sprintf("sudo wrangl runner bootstrap --controller %s --token %s", d.Fleet.EnrollURL, tok),
		Binary:  strings.TrimSuffix(d.Fleet.EnrollURL, "/") + proto.BinaryPath(uname)}, nil
}

func runnerInstanceIDs(r *http.Request, d Deps) (map[string]bool, error) {
	rows, err := d.Fleet.DB.RQ.ListRunners(r.Context(), 0)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, x := range rows {
		if x.Runner.InstanceID.Valid && x.Runner.State != "terminated" {
			out[x.Runner.InstanceID.String] = true
		}
	}
	return out, nil
}

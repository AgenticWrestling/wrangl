package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nreid/devhost/cmd/wrangl/internal/controller/costs"
	"github.com/nreid/devhost/cmd/wrangl/internal/errs"
	"github.com/nreid/devhost/cmd/wrangl/internal/proto"
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

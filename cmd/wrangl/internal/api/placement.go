package api

import (
	"context"
	"net/http"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/placement"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// needView is the effective need set for a project (taskID 0) or a task.
func needView(ctx context.Context, d Deps, projectID, taskID int64) *proto.NeedView {
	r, err := (&placement.Service{DB: d.Fleet.DB}).Resolve(ctx, projectID, taskID)
	if err != nil {
		return nil
	}
	v, m, k := r.View()
	return &proto.NeedView{VCPU: v, RAM: m, Disk: k, Source: r.Source}
}

func registerPlacement(mux *http.ServeMux, d Deps) {
	svc := &placement.Service{DB: d.Fleet.DB}
	view := func(r *http.Request) (proto.PlacementView, error) {
		g, err := svc.Global(r.Context())
		if err != nil {
			return proto.PlacementView{}, err
		}
		v, m, k := g.View()
		out := proto.PlacementView{Need: proto.NeedView{VCPU: v, RAM: m, Disk: k}}
		out.NeedsInputGraceS = int64(svc.NeedsInputGrace(r.Context()).Seconds())
		if b, err := svc.Budget(r.Context()); err == nil {
			out.Budget, out.WarnPct, out.Committed, out.Approaching = b.LimitUSD, b.WarnPct, b.Committed, b.Approaching()
		}
		return out, nil
	}
	mux.HandleFunc("GET /api/v1/placement", getJSON(view))
	mux.HandleFunc("PATCH /api/v1/placement", handle(http.StatusOK, func(r *http.Request, req proto.PlacementPatch) (proto.PlacementView, error) {
		if _, err := svc.SetGlobal(r.Context(), req.Need); err != nil {
			return proto.PlacementView{}, err
		}
		if req.NeedsInputGrace != nil {
			if err := svc.SetNeedsInputGrace(r.Context(), *req.NeedsInputGrace); err != nil {
				return proto.PlacementView{}, err
			}
		}
		if req.Budget != nil {
			if err := svc.SetBudget(r.Context(), *req.Budget); err != nil {
				return proto.PlacementView{}, err
			}
		}
		return view(r)
	}))
}

func capacityView(ctx context.Context, db *db.DB, r dbgen.Runner) *proto.CapacityView {
	c, err := (&placement.Service{DB: db}).RunnerCapacity(ctx, r)
	if err != nil {
		return nil
	}
	return &proto.CapacityView{VCPU: c.VCPU, RAMMiB: c.RAMMiB, DiskGiB: c.DiskGiB, UsedVCPU: c.UsedVCPU, UsedRAMMiB: c.UsedRAMMiB,
		UsedDiskGiB: c.UsedDiskGiB, MaxSessions: c.MaxSessions, Explicit: c.Explicit, GuardClosed: c.GuardClosed, GuardReason: c.GuardReason}
}

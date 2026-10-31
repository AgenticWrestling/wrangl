package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nreid/devhost/cmd/wrangl/internal/controller/natsd"
	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbgen"
	"github.com/nreid/devhost/cmd/wrangl/internal/proto"
)

// ResendApplyAfter: a runner still provisioning, not applying and behind is re-sent the
// template after this long (covers a daemon restart mid-apply).
const ResendApplyAfter = 60 * time.Second

// UpdateTemplate replaces a template, bumps its version and clears any failure. Runners
// pick the new version up on their next heartbeat.
func (s *Service) UpdateTemplate(ctx context.Context, name string, yamlBody []byte) (dbgen.RunnerTemplate, error) {
	p, err := ParseTemplate(yamlBody)
	if err != nil {
		return dbgen.RunnerTemplate{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if p.Name != name {
		return dbgen.RunnerTemplate{}, fmt.Errorf("%w: file names template %q, not %q (templates cannot be renamed)", ErrInvalid, p.Name, name)
	}
	t, err := s.DB.Q.UpdateTemplate(ctx, dbgen.UpdateTemplateParams{
		Description: p.Description, MiseToml: p.MiseToml, SetupScript: p.SetupScript, AgentJson: p.AgentJson,
		MinVcpu: p.MinVcpu, MinRamMib: p.MinRamMib, MinDiskGib: p.MinDiskGib, Arch: p.Arch,
		InstanceTypes: p.InstanceTypes, Ami: p.Ami, MaxRunners: p.MaxRunners, IdleTimeoutS: p.IdleTimeoutS, Name: name,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("%w: template %q", ErrNotFound, name)
	}
	if err == nil {
		s.Log.Info("template updated", "template", name, "version", t.Version)
	}
	return t, err
}

// reconcileTemplate runs after every heartbeat: if the runner is behind its template and
// nothing is applying, claim it (provisioning) and send cmd.template.
func (s *Service) reconcileTemplate(ctx context.Context, id int64, hb proto.Heartbeat) error {
	r, err := s.DB.RQ.GetRunner(ctx, id)
	if err != nil {
		return err
	}
	t, err := s.DB.RQ.GetTemplate(ctx, r.TemplateID)
	if err != nil {
		return err
	}
	now := s.now()
	if hb.TemplateVersion == t.Version {
		if r.State == "provisioning" && !hb.Applying {
			// Applied, but the result event never arrived (e.g. the runner restarted
			// between writing its state and publishing). Trust the heartbeat.
			return s.DB.Q.TemplateApplied(ctx, dbgen.TemplateAppliedParams{
				Version: t.Version, Log: r.TemplateLog, Now: nullStr(ts(now)), ID: id,
			})
		}
		return nil
	}
	if hb.Applying || t.FailedVersion == t.Version || s.NC == nil {
		return nil
	}
	if _, err := s.DB.Q.StartProvisioning(ctx, dbgen.StartProvisioningParams{
		Now: nullStr(ts(now)), ID: id, ResendBefore: nullStr(ts(now.Add(-ResendApplyAfter))),
	}); errors.Is(err, sql.ErrNoRows) {
		return nil // not in a state that may apply, or recently sent
	} else if err != nil {
		return err
	}
	req, _ := json.Marshal(proto.TemplateApply{Version: t.Version, MiseTOML: t.MiseToml, SetupScript: t.SetupScript})
	msg, err := s.NC.RequestWithContext(ctx, natsd.RunnerCmdSubject(id, "template"), req)
	if err != nil {
		return fmt.Errorf("send template to %s: %w", r.Name, err)
	}
	var ack proto.TemplateAck
	if json.Unmarshal(msg.Data, &ack) != nil || !ack.Accepted {
		return fmt.Errorf("%s declined template v%d: %s", r.Name, t.Version, ack.Reason)
	}
	s.Log.Info("template sent", "runner", r.Name, "template", t.Name, "version", t.Version)
	return nil
}

// TemplateResult records the outcome of an apply reported on wr.evt.<id>.template.
func (s *Service) TemplateResult(ctx context.Context, subject string, data []byte) error {
	parts := strings.Split(subject, ".")
	if len(parts) != 4 || parts[0] != "wr" || parts[1] != "evt" || parts[3] != "template" {
		return fmt.Errorf("%w: template subject %q", ErrInvalid, subject)
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("%w: template subject %q", ErrInvalid, subject)
	}
	var res proto.TemplateResult
	if err := json.Unmarshal(data, &res); err != nil {
		return fmt.Errorf("%w: template result: %v", ErrInvalid, err)
	}
	r, err := s.DB.RQ.GetRunner(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	now := nullStr(ts(s.now()))
	if res.OK {
		s.Log.Info("template applied", "runner", r.Name, "version", res.Version)
		return s.DB.Q.TemplateApplied(ctx, dbgen.TemplateAppliedParams{Version: res.Version, Log: res.Log, Now: now, ID: id})
	}
	s.Log.Error("template apply failed; runner cordoned and template version marked failed",
		"runner", r.Name, "version", res.Version, "err", res.Error)
	return s.DB.Tx(ctx, func(q *dbgen.Queries) error {
		if err := q.TemplateFailed(ctx, dbgen.TemplateFailedParams{Error: res.Error, Log: res.Log, Now: now, ID: id}); err != nil {
			return err
		}
		return q.MarkTemplateFailed(ctx, dbgen.MarkTemplateFailedParams{Version: res.Version, ID: r.TemplateID})
	})
}

// Cordon stops new work going to a runner (the idle reaper still applies).
func (s *Service) Cordon(ctx context.Context, name string, on bool) error {
	v := int64(0)
	if on {
		v = 1
	}
	n, err := s.DB.Q.SetCordon(ctx, dbgen.SetCordonParams{Cordoned: v, Name: name})
	if err == nil && n == 0 {
		return fmt.Errorf("%w: runner %q", ErrNotFound, name)
	}
	return err
}

// Drain cordons a runner and terminates it once idle (controller-launched only).
func (s *Service) Drain(ctx context.Context, name string) error {
	n, err := s.DB.Q.RequestDrain(ctx, name)
	if err == nil && n == 0 {
		return fmt.Errorf("%w: runner %q", ErrNotFound, name)
	}
	return err
}

// SweepDrained terminates drained, launched runners that have nothing to do.
func (s *Service) SweepDrained(ctx context.Context) ([]string, error) {
	cands, err := s.DB.RQ.DrainCandidates(ctx)
	if err != nil {
		return nil, err
	}
	var done []string
	var errs []error
	for _, c := range cands {
		if err := s.terminate(ctx, c.ID, c.Name, c.InstanceID.String, "drained"); err != nil {
			errs = append(errs, err)
			continue
		}
		done = append(done, c.Name)
	}
	return done, errors.Join(errs...)
}

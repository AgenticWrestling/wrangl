package fleet

import (
	"context"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/capability"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/placement"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// setProvides stores the extra tags a template file declares.
func (s *Service) setProvides(ctx context.Context, templateID int64, provides []string) {
	tags, err := capability.ParseTags(provides)
	if err != nil {
		return // ParseTemplate already refused a bad list
	}
	s.DB.Q.DeleteTemplateCapsBySource(ctx, templateID, "provides")
	for k, v := range tags {
		s.DB.Q.UpsertTemplateCap(ctx, templateID, k, v, "provides")
	}
}

// SyncCapabilities rebuilds what templates provide (their derived tags: arch, allowed agents, mise tools;
// declared `provides:` tags are kept) and copies each template's tags to its live runners as source
// "template". Tags a runner probed or an owner set by hand are never replaced. It is cheap and safe to run
// often: at startup, when a template changes, when a runner enrols, and periodically by the scheduler (agent
// allow-lists change without touching the template).
func (s *Service) SyncCapabilities(ctx context.Context) error {
	ts, err := s.DB.RQ.ListTemplates(ctx)
	if err != nil {
		return err
	}
	for _, t := range ts {
		var agents []string
		if as, err := s.DB.RQ.ListTemplateAgents(ctx, t.ID); err == nil {
			for _, a := range as {
				agents = append(agents, a.Name)
			}
		}
		s.DB.Q.DeleteTemplateCapsBySource(ctx, t.ID, "derived")
		for k, v := range placement.DeriveTemplateTags(t.Arch, t.MiseToml, agents) {
			s.DB.Q.AddTemplateCapIfAbsent(ctx, t.ID, k, v, "derived")
		}
	}
	tags, err := placement.TemplateTags(ctx, s.DB.RQ)
	if err != nil {
		return err
	}
	rows, err := s.DB.RQ.ListRunners(ctx, 0)
	if err != nil {
		return err
	}
	for _, row := range rows {
		r := row.Runner
		s.DB.Q.DeleteRunnerCapsBySource(ctx, r.ID, "template")
		for k, v := range tags[r.TemplateID] {
			s.DB.Q.AddRunnerCapIfAbsent(ctx, r.ID, k, v, "template")
		}
	}
	return nil
}

// recordProbe stores the tags a runner measured about itself at enrolment (tool:* and agent:* only: the
// machine can vouch for what is installed on it, not for its arch or role, which come from the template).
func (s *Service) recordProbe(ctx context.Context, runnerID int64, f proto.Facts) {
	for _, raw := range f.Tags {
		k, v, err := capability.ParseTag(raw)
		if err != nil {
			continue
		}
		if strings.HasPrefix(k, "tool:") || strings.HasPrefix(k, "agent:") {
			s.DB.Q.UpsertRunnerCap(ctx, runnerID, k, v, "probe")
		}
	}
}

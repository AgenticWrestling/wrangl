package placement

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/capability"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

// Owner kinds of a requirement.
const (
	OwnerProject = "project"
	OwnerTask    = "task"
	OwnerRun     = "run"
)

// Requirements returns what an owner (project, task or queued run) requires.
func Requirements(ctx context.Context, q *dbgen.Queries, kind string, id int64) ([]capability.Requirement, error) {
	rows, err := q.ListRequirements(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	out := make([]capability.Requirement, 0, len(rows))
	for _, r := range rows {
		out = append(out, capability.Requirement{Key: r.Key, Op: r.Op, Value: r.Value})
	}
	return out, nil
}

// Required resolves what a task needs of a runner: the project's requirements, with the task's own on top
// (a task's requirement on the same key replaces the project's; keys the task does not mention still apply).
func Required(ctx context.Context, q *dbgen.Queries, projectID, taskID int64) ([]capability.Requirement, error) {
	byKey := map[string]capability.Requirement{}
	var order []string
	for _, owner := range []struct {
		kind string
		id   int64
	}{{OwnerProject, projectID}, {OwnerTask, taskID}} {
		rs, err := Requirements(ctx, q, owner.kind, owner.id)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			if _, ok := byKey[r.Key]; !ok {
				order = append(order, r.Key)
			}
			byKey[r.Key] = r
		}
	}
	out := make([]capability.Requirement, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out, nil
}

// SnapshotRequirements writes the task's resolved requirements onto the run, like the need snapshot: later
// edits affect runs queued afterwards, not queued or running ones.
func SnapshotRequirements(ctx context.Context, q *dbgen.Queries, run dbgen.Run) error {
	t, err := q.GetTask(ctx, run.TaskID)
	if err != nil {
		return err
	}
	rs, err := Required(ctx, q, t.ProjectID, t.ID)
	if err != nil {
		return err
	}
	if err := q.DeleteRequirements(ctx, OwnerRun, run.ID); err != nil {
		return err
	}
	for _, r := range rs {
		if err := q.SetRequirement(ctx, OwnerRun, run.ID, r.Key, r.Op, r.Value); err != nil {
			return err
		}
	}
	return nil
}

// EditRequirements adds (parsed, validated) and removes requirements on a project or task. A key is
// removed with just its name.
func (s *Service) EditRequirements(ctx context.Context, kind string, id int64, add, remove []string) error {
	var parsed []capability.Requirement
	for _, a := range add {
		r, err := capability.Parse(a)
		if err != nil {
			return err
		}
		parsed = append(parsed, r)
	}
	for _, r := range parsed {
		if err := s.DB.Q.SetRequirement(ctx, kind, id, r.Key, r.Op, r.Value); err != nil {
			return err
		}
	}
	for _, k := range remove {
		k = strings.TrimSpace(k)
		if k == "" {
			return fmt.Errorf("%w: give the key of the requirement to remove", errs.Invalid)
		}
		if err := s.DB.Q.DeleteRequirement(ctx, kind, id, k); err != nil {
			return err
		}
	}
	return nil
}

// RequirementStrings lists an owner's requirements in canonical text.
func (s *Service) RequirementStrings(ctx context.Context, kind string, id int64) []string {
	rs, _ := Requirements(ctx, s.DB.RQ, kind, id)
	out := []string{}
	for _, r := range rs {
		out = append(out, r.String())
	}
	return out
}

var miseTool = regexp.MustCompile(`^\s*"?([A-Za-z0-9_:./@-]+)"?\s*=\s*["']?([^"'\s#]*)["']?`)

// DeriveTemplateTags are the capabilities a template provides by construction: its arch, each agent on
// its allow-list (`agent:claude`) and each tool in the [tools] table of its mise config
// (`tool:go=1.22`; a non-numeric version such as "latest" leaves the tag bare).
func DeriveTemplateTags(arch, miseToml string, agents []string) capability.Tags {
	t := capability.Tags{"arch": arch}
	for _, a := range agents {
		t["agent:"+strings.ToLower(a)] = ""
	}
	inTools := false
	for _, line := range strings.Split(miseToml, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "[") {
			inTools = l == "[tools]"
			continue
		}
		if !inTools || l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if m := miseTool.FindStringSubmatch(l); m != nil {
			name := strings.ToLower(m[1])
			if i := strings.LastIndex(name, ":"); i >= 0 {
				name = name[i+1:]
			}
			v := m[2]
			if capability.IsVersion(v) {
				t["tool:"+name] = v
			} else {
				t["tool:"+name] = ""
			}
		}
	}
	return t
}

// TemplateTags and RunnerTags read the stored tags, keyed by template or runner id.
func TemplateTags(ctx context.Context, q *dbgen.Queries) (map[int64]capability.Tags, error) {
	rows, err := q.ListTemplateCaps(ctx)
	if err != nil {
		return nil, err
	}
	out := map[int64]capability.Tags{}
	for _, r := range rows {
		if out[r.TemplateID] == nil {
			out[r.TemplateID] = capability.Tags{}
		}
		out[r.TemplateID][r.Key] = r.Value
	}
	return out, nil
}

func RunnerTags(ctx context.Context, q *dbgen.Queries) (map[int64]capability.Tags, error) {
	rows, err := q.ListRunnerCaps(ctx)
	if err != nil {
		return nil, err
	}
	out := map[int64]capability.Tags{}
	for _, r := range rows {
		if out[r.RunnerID] == nil {
			out[r.RunnerID] = capability.Tags{}
		}
		out[r.RunnerID][r.Key] = r.Value
	}
	return out, nil
}

// TemplateProvides returns the extra tags templates declare with `provides:`, as "key=value" strings.
func TemplateProvides(ctx context.Context, q *dbgen.Queries) (map[int64][]string, error) {
	rows, err := q.ListTemplateCaps(ctx)
	if err != nil {
		return nil, err
	}
	out := map[int64][]string{}
	for _, r := range rows {
		if r.Source != "provides" {
			continue
		}
		if r.Value == "" {
			out[r.TemplateID] = append(out[r.TemplateID], r.Key)
		} else {
			out[r.TemplateID] = append(out[r.TemplateID], r.Key+"="+r.Value)
		}
	}
	return out, nil
}

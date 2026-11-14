package plans

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

func twoRepoPlanEnv(t *testing.T) (*Service, *db.DB, *stack.Service) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, q := range []string{
		`INSERT INTO runner_template (name, mise_toml, agent_json, min_vcpu, min_ram_mib, min_disk_gib) VALUES ('go','','{}',1,1,1)`,
		`INSERT INTO project (slug, name, default_template_id) VALUES ('p','p',1)`,
		`INSERT INTO repo (project_id, owner, name) VALUES (1,'o','a'), (1,'o','b')`,
	} {
		if _, err := d.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	sq, _ := stack.NewSqids(stack.DefaultSqidsAlphabet)
	st := &stack.Service{DB: d, Sqids: sq}
	return &Service{DB: d, Stack: st, Log: slog.Default()}, d, st
}

// A plan run on a two-repo project needs a repo: the project default provides one; a proposal's subtasks
// may name theirs, which must be linked; accepting the split sets it on the new tasks.
func TestPlanRunRepoAndProposalRepos(t *testing.T) {
	s, d, st := twoRepoPlanEnv(t)
	ctx := context.Background()
	task, err := st.Create(ctx, stack.CreateParams{Project: "p", Title: "plan me", Type: "milestone", Actor: stack.User})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Queue(ctx, task.Sqid.String, ""); !errors.Is(err, errs.Invalid) {
		t.Fatalf("two repos and no default: %v", err)
	}
	d.W.Exec(`UPDATE project SET default_repo_id = 2`)
	run, err := s.Queue(ctx, task.Sqid.String, "")
	if err != nil || run.RepoID.Int64 != 2 {
		t.Fatalf("plan run: %+v %v", run, err)
	}

	sc := runs.Scope{RunID: run.ID, Kind: "plan", RootID: task.ID, ProjectID: 1}
	bad, _ := json.Marshal(Proposal{Children: []proto.SplitNode{{Title: "x", Repo: "o/nope"}}})
	_, err = s.ProposeSplit(ctx, sc, bad)
	if !errors.Is(err, errs.Invalid) || !strings.Contains(err.Error(), "o/a, o/b") {
		t.Fatalf("unlinked repo in a proposal: %v", err)
	}

	// Accepting a split applies the repo per subtask.
	if _, err := st.Split(ctx, task.Sqid.String, toStack([]proto.SplitNode{{Title: "api", Repo: "o/a"}, {Title: "web"}}), false, false, stack.User); err != nil {
		t.Fatal(err)
	}
	var api, web int64
	d.R.QueryRow(`SELECT COALESCE(repo_id, 0) FROM task WHERE title = 'api'`).Scan(&api)
	d.R.QueryRow(`SELECT COALESCE(repo_id, 0) FROM task WHERE title = 'web'`).Scan(&web)
	if api != 1 || web != 0 {
		t.Fatalf("subtask repos: api %d web %d", api, web)
	}
}

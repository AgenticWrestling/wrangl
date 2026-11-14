package scheduler

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
)

func readRepoFixture(t *testing.T, repos int) (*Scheduler, *gh.Fake, func() []string) {
	s, _, _, d := setup(t, 1)
	for i := 0; i < repos; i++ {
		note := ""
		if i == 1 {
			note = "web client"
		}
		d.W.Exec(`INSERT INTO repo (project_id, owner, name, default_branch, gh_installation_id, clone_url, note) VALUES (1, 'o', ?, 'main', 5, ?, ?)`,
			fmt.Sprintf("r%02d", i), fmt.Sprintf("file:///r%02d", i), note)
	}
	queue(t, d, 1)
	d.W.Exec(`UPDATE run SET template_id = 1, repo_id = 1`)
	fake := gh.NewFake()
	s.GH = fake
	return s, fake, func() []string {
		rows, _ := d.RQ.QueuedRunsForAssignment(context.Background())
		own, _ := d.RQ.GetRepo(context.Background(), 1)
		var out []string
		for _, rr := range s.readRepos(context.Background(), rows[0], own) {
			out = append(out, rr.Repo+"|"+rr.Name+"|"+rr.Note+"|"+rr.Token)
		}
		return out
	}
}

func TestReadReposOfATwoRepoProject(t *testing.T) {
	_, fake, got := readRepoFixture(t, 2)
	out := got()
	if len(out) != 1 || !strings.HasPrefix(out[0], "o/r01|r01|web client|ghs_fakeread_r01") {
		t.Fatalf("read repos: %v", out)
	}
	if fake.ReadTokens != 1 || fake.Tokens != 0 {
		t.Fatalf("tokens: %d read, %d write; a read checkout must only get a read token", fake.ReadTokens, fake.Tokens)
	}
}

func TestReadReposNoneForOneRepoOrWhenSwitchedOff(t *testing.T) {
	_, _, got := readRepoFixture(t, 1)
	if out := got(); len(out) != 0 {
		t.Fatalf("single repo: %v", out)
	}
	s, _, got2 := readRepoFixture(t, 3)
	s.Fleet.DB.W.Exec(`UPDATE project SET read_repos = 'none'`)
	if out := got2(); len(out) != 0 {
		t.Fatalf("read_repos none: %v", out)
	}
}

func TestReadReposCappedAtEight(t *testing.T) {
	_, _, got := readRepoFixture(t, 12)
	if out := got(); len(out) != MaxReadRepos {
		t.Fatalf("%d read repos, want %d", len(out), MaxReadRepos)
	}
}

// Plan and review runs read the other repos like work runs do.
func TestReadReposForPlanAndReviewRuns(t *testing.T) {
	for _, kind := range []string{"plan", "review"} {
		s, _, got := readRepoFixture(t, 2)
		s.Fleet.DB.W.Exec(`UPDATE run SET kind = ?`, kind)
		if out := got(); len(out) != 1 {
			t.Fatalf("%s run: %v", kind, out)
		}
	}
}

package gh

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Fake is an in-memory GitHub for tests. Repos are added with AddRepo; their CloneURL can
// point at a local bare repository (file://...).
type Fake struct {
	mu     sync.Mutex
	repos  map[string]Repo
	issues map[string][]Issue
	Tokens int // RepoToken calls
}

// NewFake returns an empty fake.
func NewFake() *Fake { return &Fake{repos: map[string]Repo{}, issues: map[string][]Issue{}} }

// AddRepo makes the App "installed" on r.
func (f *Fake) AddRepo(r Repo, issues ...Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.InstallationID == 0 {
		r.InstallationID = int64(1000 + len(f.repos))
	}
	f.repos[r.Owner+"/"+r.Name] = r
	f.issues[r.Owner+"/"+r.Name] = issues
}

// Repo implements API.
func (f *Fake) Repo(_ context.Context, owner, name string) (Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.repos[owner+"/"+name]
	if !ok {
		return Repo{}, fmt.Errorf("%s/%s: %w", owner, name, ErrNotInstalled)
	}
	return r, nil
}

// RepoToken implements API.
func (f *Fake) RepoToken(_ context.Context, r Repo) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Tokens++
	return fmt.Sprintf("ghs_fake_%s_%d", r.Name, f.Tokens), time.Now().Add(time.Hour), nil
}

// Issues implements API.
func (f *Fake) Issues(_ context.Context, r Repo, state string) ([]Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Issue
	for _, i := range f.issues[r.Owner+"/"+r.Name] {
		if state == "all" || i.State == state {
			out = append(out, i)
		}
	}
	return out, nil
}

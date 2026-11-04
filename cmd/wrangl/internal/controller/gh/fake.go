package gh

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory GitHub for tests. Repos are added with AddRepo; their CloneURL can
// point at a local bare repository (file://...).
type Fake struct {
	mu        sync.Mutex
	repos     map[string]Repo
	issues    map[string][]Issue
	Tokens    int // RepoToken calls
	prs       map[int64]*fakePR
	nextPR    int64
	checks    map[string]string
	defChecks string
	issuePRs  map[string]bool
	comments  map[string][]IssueComment
	calls     FakeCalls
	writes    []FakeWrite
	writeErr  error
	tick      int
}

// NewFake returns an empty fake.
func NewFake() *Fake {
	return &Fake{repos: map[string]Repo{}, issues: map[string][]Issue{}, prs: map[int64]*fakePR{}, checks: map[string]string{}}
}

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
		return Repo{}, &NotInstalledError{Owner: owner, Name: name}
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

// ListRepos implements API.
func (f *Fake) ListRepos(_ context.Context, owner string) ([]Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Repo
	for _, r := range f.repos {
		if strings.EqualFold(r.Owner, owner) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

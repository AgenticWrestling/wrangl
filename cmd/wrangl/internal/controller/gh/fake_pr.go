package gh

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// fakePR is a PR in the fake.
type fakePR struct {
	PR
	repo       string
	head, base string
	title      string
	fork       bool
	reviews    []string // "EVENT: body"
}

func (f *Fake) bare(r Repo) (string, bool) {
	if strings.HasPrefix(r.CloneURL, "file://") {
		return strings.TrimPrefix(r.CloneURL, "file://"), true
	}
	return "", false
}

func gitOut(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"--git-dir", dir}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// SetChecks sets the CI result the fake reports for a commit ("" = default).
func (f *Fake) SetChecks(sha, state string) { f.mu.Lock(); f.checks[sha] = state; f.mu.Unlock() }

// DefaultChecks is reported for commits without SetChecks (default success).
func (f *Fake) SetDefaultChecks(state string) { f.mu.Lock(); f.defChecks = state; f.mu.Unlock() }

// Reviews returns the reviews posted on a PR.
func (f *Fake) Reviews(n int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.prs[n]; p != nil {
		return append([]string(nil), p.reviews...)
	}
	return nil
}

// FakePR returns a PR's current state.
func (f *Fake) FakePR(n int64) (PR, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.prs[n]; p != nil {
		return p.PR, true
	}
	return PR{}, false
}

func (f *Fake) refresh(p *fakePR) {
	if dir, ok := f.bare(f.repos[p.repo]); ok && !p.Merged {
		if sha, err := gitOut(dir, "rev-parse", "refs/heads/"+p.head); err == nil {
			p.HeadSHA = sha
		}
	}
}

// CreatePR implements PRAPI.
func (f *Fake) CreatePR(_ context.Context, r Repo, head, base, title, body string) (PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.prs {
		if p.repo == r.Owner+"/"+r.Name && p.head == head && p.State == "open" {
			return PR{}, fmt.Errorf("github: HTTP 422: a pull request already exists for %s", head)
		}
	}
	f.nextPR++
	p := &fakePR{PR: PR{Number: f.nextPR, URL: fmt.Sprintf("https://github.com/%s/%s/pull/%d", r.Owner, r.Name, f.nextPR), State: "open"},
		repo: r.Owner + "/" + r.Name, head: head, base: base, title: title}
	f.prs[p.Number] = p
	f.refresh(p)
	return p.PR, nil
}

func (f *Fake) get(n int64) (*fakePR, error) {
	p := f.prs[n]
	if p == nil {
		return nil, fmt.Errorf("github: HTTP 404: no PR %d", n)
	}
	f.refresh(p)
	return p, nil
}

// GetPR implements PRAPI.
func (f *Fake) GetPR(_ context.Context, _ Repo, n int64) (PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.get(n)
	if err != nil {
		return PR{}, err
	}
	pr := p.PR
	if pr.Mergeable == "" {
		pr.Mergeable = MergeClean
	}
	return pr, nil
}

// SetMergeable sets the mergeable state the fake reports for a PR (default clean).
func (f *Fake) SetMergeable(n int64, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.prs[n]; p != nil {
		p.Mergeable = state
	}
}

// PRFiles implements PRAPI (computed from the local bare repo).
func (f *Fake) PRFiles(_ context.Context, r Repo, n int64) ([]PRFile, error) {
	f.mu.Lock()
	p, err := f.get(n)
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	dir, ok := f.bare(r)
	if !ok {
		return nil, nil
	}
	out, err := gitOut(dir, "diff", "--numstat", p.base+"..."+p.head)
	if err != nil {
		return nil, fmt.Errorf("fake diff: %v: %s", err, out)
	}
	var files []PRFile
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Fields(line)
		if len(parts) == 3 {
			a, _ := strconv.Atoi(parts[0])
			d, _ := strconv.Atoi(parts[1])
			files = append(files, PRFile{Path: parts[2], Additions: a, Deletions: d})
		}
	}
	return files, nil
}

// PRDiff implements PRAPI.
func (f *Fake) PRDiff(_ context.Context, r Repo, n int64) (string, error) {
	f.mu.Lock()
	p, err := f.get(n)
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	dir, ok := f.bare(r)
	if !ok {
		return "", nil
	}
	return gitOut(dir, "diff", p.base+"..."+p.head)
}

// Checks implements PRAPI.
func (f *Fake) Checks(_ context.Context, _ Repo, sha string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.checks[sha]; ok {
		return s, nil
	}
	if f.defChecks != "" {
		return f.defChecks, nil
	}
	return ChecksSuccess, nil
}

// Review implements PRAPI.
func (f *Fake) Review(_ context.Context, _ Repo, n int64, event, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.get(n)
	if err != nil {
		return err
	}
	p.reviews = append(p.reviews, event+": "+body)
	return nil
}

// Merge implements PRAPI: fast-forwards the base branch to the head in the bare repo.
func (f *Fake) Merge(_ context.Context, r Repo, n int64, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.get(n)
	if err != nil {
		return err
	}
	if p.State != "open" {
		return fmt.Errorf("github: HTTP 405: PR %d is not open", n)
	}
	if dir, ok := f.bare(r); ok {
		if out, err := gitOut(dir, "update-ref", "refs/heads/"+p.base, p.HeadSHA); err != nil {
			return fmt.Errorf("fake merge: %v: %s", err, out)
		}
	}
	p.State, p.Merged = "closed", true
	return nil
}

// ClosePR implements PRAPI.
func (f *Fake) ClosePR(_ context.Context, _ Repo, n int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.get(n)
	if err != nil {
		return err
	}
	p.State = "closed"
	return nil
}

// SetFork marks a PR as coming from a fork.
func (f *Fake) SetFork(n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.prs[n]; p != nil {
		p.fork = true
	}
}

// OpenPRs implements PRAPI.
func (f *Fake) OpenPRs(_ context.Context, r Repo) ([]OpenPR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []OpenPR
	for _, p := range f.prs {
		if p.repo == r.Owner+"/"+r.Name && p.State == "open" {
			f.refresh(p)
			out = append(out, OpenPR{PR: p.PR, Title: p.title, HeadBranch: p.head, Fork: p.fork})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

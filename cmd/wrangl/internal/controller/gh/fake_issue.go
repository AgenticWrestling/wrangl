package gh

import (
	"context"
	"strings"
)

// FakeCalls counts read calls the fake served, so tests can assert "no GitHub call was made".
type FakeCalls struct{ GetIssue, Comments, Search int }

// AddIssuePR registers a pull request number in r so GetIssue answers ErrIsPullRequest for it.
func (f *Fake) AddIssuePR(r Repo, number int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.issuePRs == nil {
		f.issuePRs = map[string]bool{}
	}
	f.issuePRs[key(r, number)] = true
}

// AddComments sets the comments of an issue.
func (f *Fake) AddComments(r Repo, number int64, cs ...IssueComment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.comments == nil {
		f.comments = map[string][]IssueComment{}
	}
	f.comments[key(r, number)] = cs
}

// IssueCalls returns the read calls served so far.
func (f *Fake) IssueCalls() FakeCalls {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func key(r Repo, n int64) string { return r.Owner + "/" + r.Name + "#" + itoa(n) }

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// GetIssue implements IssueReader.
func (f *Fake) GetIssue(_ context.Context, r Repo, number int64) (Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.GetIssue++
	if f.issuePRs[key(r, number)] {
		return Issue{}, ErrIsPullRequest
	}
	for _, i := range f.issues[r.Owner+"/"+r.Name] {
		if i.Number == number {
			return i, nil
		}
	}
	return Issue{}, ErrIssueNotFound
}

// IssueComments implements IssueReader (one page).
func (f *Fake) IssueComments(_ context.Context, r Repo, number int64, _ int) ([]IssueComment, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.Comments++
	return f.comments[key(r, number)], false, nil
}

// SearchIssues implements IssueReader: every word must appear in the title or body.
func (f *Fake) SearchIssues(_ context.Context, r Repo, query, state string, limit int) ([]Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.Search++
	var out []Issue
	for _, i := range f.issues[r.Owner+"/"+r.Name] {
		if state != "" && state != "all" && i.State != state {
			continue
		}
		text := strings.ToLower(i.Title + " " + i.Body)
		ok := true
		for _, w := range strings.Fields(strings.ToLower(query)) {
			ok = ok && strings.Contains(text, w)
		}
		if ok {
			out = append(out, i)
		}
	}
	return out, nil
}

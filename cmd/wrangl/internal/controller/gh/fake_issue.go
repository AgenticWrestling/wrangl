package gh

import (
	"context"
	"fmt"
	"strings"
	"time"
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

// FakeWrite records one write the fake served.
type FakeWrite struct {
	Op     string // create | edit | comment | close
	Repo   string
	Number int64
	Body   string
}

// Writes returns the writes served so far, in order.
func (f *Fake) Writes() []FakeWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeWrite(nil), f.writes...)
}

// FailWrites makes every write fail with err (nil restores normal behaviour).
func (f *Fake) FailWrites(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeErr = err
}

// stamp returns a fresh updated_at, later than every earlier stamp and than the 10:00 used by tests.
func (f *Fake) stamp() string {
	f.tick++
	return time.Date(2026, 11, 4, 11, 0, 0, 0, time.UTC).Add(time.Duration(f.tick) * time.Second).Format("2006-01-02T15:04:05Z")
}

func (f *Fake) find(r Repo, n int64) (*Issue, error) {
	if f.issuePRs[key(r, n)] {
		return nil, ErrIsPullRequest
	}
	list := f.issues[r.Owner+"/"+r.Name]
	for i := range list {
		if list[i].Number == n {
			return &list[i], nil
		}
	}
	return nil, ErrIssueNotFound
}

// CreateIssue implements IssueWriter.
func (f *Fake) CreateIssue(_ context.Context, r Repo, title, body string, labels []string) (Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return Issue{}, f.writeErr
	}
	k := r.Owner + "/" + r.Name
	var next int64 = 1
	for _, i := range f.issues[k] {
		next = max(next, i.Number+1)
	}
	for pk := range f.issuePRs {
		var n int64
		if rest, ok := strings.CutPrefix(pk, k+"#"); ok {
			fmt.Sscanf(rest, "%d", &n)
			next = max(next, n+1)
		}
	}
	now := f.stamp()
	i := Issue{Number: next, Title: title, Body: body, State: "open", URL: fmt.Sprintf("https://github.com/%s/issues/%d", k, next),
		Author: "wrangl[bot]", Labels: append([]string{}, labels...), CreatedAt: now, UpdatedAt: now}
	f.issues[k] = append(f.issues[k], i)
	f.writes = append(f.writes, FakeWrite{Op: "create", Repo: k, Number: next, Body: body})
	return i, nil
}

// EditIssue implements IssueWriter.
func (f *Fake) EditIssue(_ context.Context, r Repo, n int64, e IssueEdit) (Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return Issue{}, f.writeErr
	}
	i, err := f.find(r, n)
	if err != nil {
		return Issue{}, err
	}
	if e.Title != nil {
		i.Title = *e.Title
	}
	if e.Body != nil {
		i.Body = *e.Body
	}
	i.Labels = ApplyLabels(i.Labels, e.AddLabels, e.RemoveLabels)
	i.UpdatedAt = f.stamp()
	f.writes = append(f.writes, FakeWrite{Op: "edit", Repo: r.Owner + "/" + r.Name, Number: n})
	return *i, nil
}

// CreateComment implements IssueWriter.
func (f *Fake) CreateComment(_ context.Context, r Repo, n int64, body string) (IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return IssueComment{}, f.writeErr
	}
	i, err := f.find(r, n)
	if err != nil {
		return IssueComment{}, err
	}
	i.Comments++
	i.UpdatedAt = f.stamp()
	c := IssueComment{ID: int64(9000 + len(f.writes)), Author: "wrangl[bot]", Body: body, CreatedAt: i.UpdatedAt,
		URL: fmt.Sprintf("%s#issuecomment-%d", i.URL, 9000+len(f.writes))}
	f.comments[key(r, n)] = append(f.comments[key(r, n)], c)
	f.writes = append(f.writes, FakeWrite{Op: "comment", Repo: r.Owner + "/" + r.Name, Number: n, Body: body})
	return c, nil
}

// CloseIssue implements IssueWriter.
func (f *Fake) CloseIssue(_ context.Context, r Repo, n int64, _ string) (Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return Issue{}, f.writeErr
	}
	i, err := f.find(r, n)
	if err != nil {
		return Issue{}, err
	}
	i.State = "closed"
	i.UpdatedAt = f.stamp()
	f.writes = append(f.writes, FakeWrite{Op: "close", Repo: r.Owner + "/" + r.Name, Number: n})
	return *i, nil
}

// MustIssues returns the fake's issues of r with the given numbers (test helper).
func (f *Fake) MustIssues(r Repo, numbers ...int64) []Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Issue
	for _, i := range f.issues[r.Owner+"/"+r.Name] {
		for _, n := range numbers {
			if i.Number == n {
				out = append(out, i)
			}
		}
	}
	return out
}

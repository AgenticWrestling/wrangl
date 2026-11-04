package gh

import (
	"context"
	"strings"
	"sync"
	"time"
)

// AppRef locates one account's App: its id and the path of its private key.
type AppRef struct {
	AppID   int64
	KeyPath string
}

// Multi routes every call to the App of the account that owns the repository. A GitHub App
// lives in one account, personal or organisation, so a controller working across several
// accounts has one App each. An owner with no App yet gets a *NoAppError.
type Multi struct {
	// Lookup finds the App for an owner (case-insensitive login).
	Lookup func(owner string) (AppRef, bool)
	// BaseURL is copied to each client (tests point it at a fake GitHub).
	BaseURL string
	// Role is "" for author Apps and "reviewer" for reviewer Apps; it only shapes errors.
	Role string

	mu      sync.Mutex
	clients map[string]*Client
	listed  map[string]listed
}

type listed struct {
	at    time.Time
	repos []Repo
}

func (m *Multi) client(owner string) (*Client, error) {
	ref, ok := m.Lookup(owner)
	if !ok || ref.AppID == 0 {
		return nil, &NoAppError{Owner: owner, Role: m.Role}
	}
	key := strings.ToLower(owner)
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.clients[key]; c != nil && c.AppID == ref.AppID {
		return c, nil
	}
	k, err := LoadKey(ref.KeyPath)
	if err != nil {
		return nil, err
	}
	c := &Client{AppID: ref.AppID, Key: k, BaseURL: m.BaseURL, Role: m.Role}
	if m.clients == nil {
		m.clients = map[string]*Client{}
	}
	m.clients[key] = c
	return c, nil
}

// Repo implements API.
func (m *Multi) Repo(ctx context.Context, owner, name string) (Repo, error) {
	c, err := m.client(owner)
	if err != nil {
		return Repo{}, err
	}
	return c.Repo(ctx, owner, name)
}

// RepoToken implements API.
func (m *Multi) RepoToken(ctx context.Context, r Repo) (string, time.Time, error) {
	c, err := m.client(r.Owner)
	if err != nil {
		return "", time.Time{}, err
	}
	return c.RepoToken(ctx, r)
}

// Issues implements API.
func (m *Multi) Issues(ctx context.Context, r Repo, state string) ([]Issue, error) {
	c, err := m.client(r.Owner)
	if err != nil {
		return nil, err
	}
	return c.Issues(ctx, r, state)
}

// ListRepos implements API, cached for two minutes per owner (it is called when a page renders).
func (m *Multi) ListRepos(ctx context.Context, owner string) ([]Repo, error) {
	key := strings.ToLower(owner)
	m.mu.Lock()
	if l, ok := m.listed[key]; ok && time.Since(l.at) < 2*time.Minute {
		m.mu.Unlock()
		return l.repos, nil
	}
	m.mu.Unlock()
	c, err := m.client(owner)
	if err != nil {
		return nil, err
	}
	repos, err := c.ListRepos(ctx, owner)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.listed == nil {
		m.listed = map[string]listed{}
	}
	m.listed[key] = listed{at: time.Now(), repos: repos}
	m.mu.Unlock()
	return repos, nil
}

// Forget drops cached clients and repository lists (after an App is created or changed).
func (m *Multi) Forget() {
	m.mu.Lock()
	m.clients, m.listed = nil, nil
	m.mu.Unlock()
}

// The pull-request surface.

func (m *Multi) CreatePR(ctx context.Context, r Repo, head, base, title, body string) (PR, error) {
	c, err := m.client(r.Owner)
	if err != nil {
		return PR{}, err
	}
	return c.CreatePR(ctx, r, head, base, title, body)
}

func (m *Multi) GetPR(ctx context.Context, r Repo, number int64) (PR, error) {
	c, err := m.client(r.Owner)
	if err != nil {
		return PR{}, err
	}
	return c.GetPR(ctx, r, number)
}

func (m *Multi) PRFiles(ctx context.Context, r Repo, number int64) ([]PRFile, error) {
	c, err := m.client(r.Owner)
	if err != nil {
		return nil, err
	}
	return c.PRFiles(ctx, r, number)
}

func (m *Multi) PRDiff(ctx context.Context, r Repo, number int64) (string, error) {
	c, err := m.client(r.Owner)
	if err != nil {
		return "", err
	}
	return c.PRDiff(ctx, r, number)
}

func (m *Multi) Checks(ctx context.Context, r Repo, sha string) (string, error) {
	c, err := m.client(r.Owner)
	if err != nil {
		return "", err
	}
	return c.Checks(ctx, r, sha)
}

func (m *Multi) Review(ctx context.Context, r Repo, number int64, event, body string) error {
	c, err := m.client(r.Owner)
	if err != nil {
		return err
	}
	return c.Review(ctx, r, number, event, body)
}

func (m *Multi) Merge(ctx context.Context, r Repo, number int64, title string) error {
	c, err := m.client(r.Owner)
	if err != nil {
		return err
	}
	return c.Merge(ctx, r, number, title)
}

func (m *Multi) ClosePR(ctx context.Context, r Repo, number int64) error {
	c, err := m.client(r.Owner)
	if err != nil {
		return err
	}
	return c.ClosePR(ctx, r, number)
}

// Has reports whether owner has an App for this Multi's role.
func (m *Multi) Has(owner string) bool {
	ref, ok := m.Lookup(owner)
	return ok && ref.AppID != 0
}

// GetIssue implements IssueReader.
func (m *Multi) GetIssue(ctx context.Context, r Repo, number int64) (Issue, error) {
	c, err := m.client(r.Owner)
	if err != nil {
		return Issue{}, err
	}
	return c.GetIssue(ctx, r, number)
}

// IssueComments implements IssueReader.
func (m *Multi) IssueComments(ctx context.Context, r Repo, number int64, page int) ([]IssueComment, bool, error) {
	c, err := m.client(r.Owner)
	if err != nil {
		return nil, false, err
	}
	return c.IssueComments(ctx, r, number, page)
}

// SearchIssues implements IssueReader.
func (m *Multi) SearchIssues(ctx context.Context, r Repo, query, state string, limit int) ([]Issue, error) {
	c, err := m.client(r.Owner)
	if err != nil {
		return nil, err
	}
	return c.SearchIssues(ctx, r, query, state, limit)
}

// CreateIssue implements IssueWriter.
func (m *Multi) CreateIssue(ctx context.Context, r Repo, title, body string, labels []string) (Issue, error) {
	c, err := m.client(r.Owner)
	if err != nil {
		return Issue{}, err
	}
	return c.CreateIssue(ctx, r, title, body, labels)
}

// EditIssue implements IssueWriter.
func (m *Multi) EditIssue(ctx context.Context, r Repo, number int64, e IssueEdit) (Issue, error) {
	c, err := m.client(r.Owner)
	if err != nil {
		return Issue{}, err
	}
	return c.EditIssue(ctx, r, number, e)
}

// CreateComment implements IssueWriter.
func (m *Multi) CreateComment(ctx context.Context, r Repo, number int64, body string) (IssueComment, error) {
	c, err := m.client(r.Owner)
	if err != nil {
		return IssueComment{}, err
	}
	return c.CreateComment(ctx, r, number, body)
}

// CloseIssue implements IssueWriter.
func (m *Multi) CloseIssue(ctx context.Context, r Repo, number int64, reason string) (Issue, error) {
	c, err := m.client(r.Owner)
	if err != nil {
		return Issue{}, err
	}
	return c.CloseIssue(ctx, r, number, reason)
}

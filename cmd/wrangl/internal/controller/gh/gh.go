// Package gh talks to GitHub as a GitHub App. API is the interface the controller uses;
// Client is the real implementation and Fake an in-memory one for tests.
package gh

import (
	"context"
	"errors"
	"time"
)

// Repo is a repository the author App is installed on.
type Repo struct {
	Owner, Name    string
	DefaultBranch  string
	CloneURL       string
	InstallationID int64
	Private        bool
}

// Issue is a GitHub issue (pull requests excluded).
type Issue struct {
	Number int64
	Title  string
	Body   string
	State  string // open | closed
	URL    string
}

// ErrNotInstalled means the App is not installed on the repository.
var ErrNotInstalled = errors.New("the GitHub App is not installed on this repository")

// API is what the controller needs from GitHub in M4. M6 adds pull requests, checks,
// reviews and merges.
type API interface {
	// Repo looks a repository up through the App's installation on it.
	Repo(ctx context.Context, owner, name string) (Repo, error)
	// RepoToken mints an installation token scoped to this one repository with
	// contents:write and pull_requests:write. It expires within the hour.
	RepoToken(ctx context.Context, r Repo) (token string, expires time.Time, err error)
	// Issues lists issues (state: open | closed | all).
	Issues(ctx context.Context, r Repo, state string) ([]Issue, error)
}

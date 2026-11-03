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

// NoAppError means no GitHub App has been created yet for the account that owns a repository.
// An App has to be created in the personal or organisation account that owns the repositories,
// so the fix is the Setup flow for that account.
type NoAppError struct{ Owner string }

func (e *NoAppError) Error() string {
	return "there is no GitHub App for " + e.Owner + " yet; create one in Settings"
}

// NotInstalledError is ErrNotInstalled with the repository it is about.
type NotInstalledError struct{ Owner, Name string }

func (e *NotInstalledError) Error() string {
	return "the GitHub App for " + e.Owner + " is not installed on " + e.Owner + "/" + e.Name
}

// Is makes errors.Is(err, ErrNotInstalled) hold.
func (e *NotInstalledError) Is(target error) bool { return target == ErrNotInstalled }

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
	// ListRepos lists the repositories an owner's App is installed on (what can be linked).
	ListRepos(ctx context.Context, owner string) ([]Repo, error)
	PRAPI
}

// PR is a pull request.
type PR struct {
	Number  int64
	URL     string
	HeadSHA string
	State   string // open | closed
	Merged  bool
}

// PRFile is one changed file.
type PRFile struct {
	Path                 string
	Additions, Deletions int
}

// Checks summarises CI on a commit.
const (
	ChecksSuccess = "success"
	ChecksPending = "pending"
	ChecksFailure = "failure"
	ChecksNone    = "none" // no checks configured
)

// PRAPI is the M6 pull-request surface. The author App creates PRs; the reviewer App
// reviews, approves and merges them (GitHub forbids approving your own PR).
type PRAPI interface {
	CreatePR(ctx context.Context, r Repo, head, base, title, body string) (PR, error)
	GetPR(ctx context.Context, r Repo, number int64) (PR, error)
	PRFiles(ctx context.Context, r Repo, number int64) ([]PRFile, error)
	PRDiff(ctx context.Context, r Repo, number int64) (string, error)
	Checks(ctx context.Context, r Repo, sha string) (string, error)
	Review(ctx context.Context, r Repo, number int64, event, body string) error // APPROVE | REQUEST_CHANGES | COMMENT
	Merge(ctx context.Context, r Repo, number int64, title string) error        // squash
	ClosePR(ctx context.Context, r Repo, number int64) error
}

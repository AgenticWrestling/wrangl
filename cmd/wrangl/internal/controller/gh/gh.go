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
	// Read-side metadata (empty when the source did not provide it).
	Author    string
	Labels    []string
	CreatedAt string
	UpdatedAt string // GitHub's updated_at, RFC3339 UTC
	Comments  int
}

// IssueComment is one comment on an issue.
type IssueComment struct {
	ID        int64
	Author    string
	Body      string
	CreatedAt string
}

// ErrIsPullRequest means the number names a pull request, not an issue. Issue tools never touch PRs.
var ErrIsPullRequest = errors.New("that number is a pull request, not an issue")

// ErrIssueNotFound means the repository has no issue with that number.
var ErrIssueNotFound = errors.New("issue not found")

// ErrNotInstalled means the App is not installed on the repository.
var ErrNotInstalled = errors.New("the GitHub App is not installed on this repository")

// NoAppError means no GitHub App has been created yet for the account that owns a repository.
// An App has to be created in the personal or organisation account that owns the repositories,
// so the fix is the Setup flow for that account.
type NoAppError struct {
	Owner string
	Role  string // "" (author) or "reviewer"
}

func (e *NoAppError) Error() string {
	if e.Role == "reviewer" {
		return "there is no reviewer App for " + e.Owner + " yet; it approves and merges pull requests. Create it in Settings"
	}
	return "there is no GitHub App for " + e.Owner + " yet; create one in Settings"
}

// NotInstalledError is ErrNotInstalled with the repository it is about.
type NotInstalledError struct {
	Owner, Name string
	Role        string // "" (author) or "reviewer"
}

func (e *NotInstalledError) Error() string {
	if e.Role == "reviewer" {
		return "the reviewer App for " + e.Owner + " is not installed on " + e.Owner + "/" + e.Name
	}
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
	IssueReader
	PRAPI
}

// IssueReader is the read-only issue surface used by the agent issue tools. It never
// returns pull requests: GetIssue answers ErrIsPullRequest for one, the others skip them.
type IssueReader interface {
	// GetIssue fetches one issue. ErrIssueNotFound / ErrIsPullRequest are returned as such.
	GetIssue(ctx context.Context, r Repo, number int64) (Issue, error)
	// IssueComments returns one page (1-based, up to 100 comments) and whether more pages follow.
	IssueComments(ctx context.Context, r Repo, number int64, page int) (comments []IssueComment, more bool, err error)
	// SearchIssues runs a text search over the repository's issues (state: open | closed | all).
	SearchIssues(ctx context.Context, r Repo, query, state string, limit int) ([]Issue, error)
}

// PR is a pull request.
type PR struct {
	Number  int64
	URL     string
	HeadSHA string
	State   string // open | closed
	Merged  bool
	// Set by ListOpenPRs (and the fake's CreatePR): what adoption needs to match a PR.
	Title    string
	Head     string // head branch name
	HeadRepo string // owner/name of the repository the head lives in ("" unknown)
	Base     string // base branch name
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
	// ListOpenPRs lists the repository's open pull requests (newest first, capped at 300).
	ListOpenPRs(ctx context.Context, r Repo) ([]PR, error)
}

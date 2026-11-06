// Personal access token mode.
//
// Every one of the 21 methods on [Client] reaches GitHub through [Client.do], and all of them
// get their Authorization header from exactly two places: [Client.appAuth] (5 call sites) and
// [Client.tokenFor] (10). That is what makes a PAT a small change rather than a second client:
// set [Client.PAT] and those two return the token, and the REST calls underneath are identical
// because the endpoints an installation token may use are the same ones a `repo`-scoped PAT may.
//
// Four methods still need their own branch, because they do not merely authenticate as the App
// -- they ask GitHub about the App itself, and with a PAT there is no App to ask about:
// Repo and ListRepos look up an installation, mintToken posts to /app/installations/..., and
// AppOwner reads /app. Those four are here.
//
// # What PAT mode cannot do, and why it is not a silent degradation
//
//   - It cannot approve a pull request. GitHub refuses to let an account approve its own PR, and
//     in PAT mode the author and the reviewer are the same account. [Client.Review] is left
//     alone so that it fails with GitHub's own 422 rather than a local pretence; link a repo with
//     a base branch that does not require an approving review (`wrangl repo link --branch`).
//   - It cannot create a repository webhook unless the token's user has admin on the repo. This
//     costs latency and not correctness: prs.Service reconciles PR state on a poll as well as on
//     a delivery, which is the path that carries PAT mode.
//   - A `repo`-scoped classic PAT is far broader and longer-lived than the per-repository,
//     hour-long, contents+pull_requests token an App mints. That is the trade, stated here
//     because nothing in the code can measure it.
package gh

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// patExpiry is what RepoToken reports for a PAT. A PAT has no expiry the API will tell us
// about, and callers cache the token until it expires, so a far-future time is the honest
// answer rather than a guess: the token stops working when it is revoked, which no expiry
// could have predicted.
var patExpiry = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)

// usesPAT reports whether this client authenticates with a personal access token.
func (c *Client) usesPAT() bool { return c.PAT != "" }

// patAuth is the Authorization value for a PAT. GitHub accepts a PAT wherever it accepts an
// installation token, under either the `Bearer` or the `token` scheme.
func (c *Client) patAuth() string { return "Bearer " + c.PAT }

// patRepo reads a repository directly, with no installation lookup.
//
// InstallationID is left zero deliberately. Nothing in PAT mode mints a token from it, and a
// fabricated id would be a number that looks meaningful in the database and in logs.
func (c *Client) patRepo(ctx context.Context, owner, name string) (Repo, error) {
	var info struct {
		DefaultBranch string `json:"default_branch"`
		CloneURL      string `json:"clone_url"`
		Private       bool   `json:"private"`
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	if _, err := c.do(ctx, http.MethodGet, path, c.patAuth(), nil, &info); err != nil {
		return Repo{}, fmt.Errorf("read %s/%s with a PAT: %w", owner, name, err)
	}
	return Repo{
		Owner: owner, Name: name,
		DefaultBranch: info.DefaultBranch, CloneURL: info.CloneURL, Private: info.Private,
	}, nil
}

// patListRepos lists the repositories the token can see under owner.
//
// This is a WIDER set than the App answer it stands in for, and the difference is worth knowing
// before trusting it as a picker: ListRepos normally means "repositories this App was installed
// on", which is an explicit grant per repository, whereas a `repo`-scoped PAT sees everything
// its user can reach in the account.
func (c *Client) patListRepos(ctx context.Context, owner string) ([]Repo, error) {
	type repoJSON struct {
		Name          string `json:"name"`
		DefaultBranch string `json:"default_branch"`
		CloneURL      string `json:"clone_url"`
		Private       bool   `json:"private"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	// Orgs first, then users: an owner is one or the other, and asking the wrong one is a 404
	// rather than an empty list, so the fallback is not a silent reduction.
	var out []Repo
	for _, base := range []string{"/orgs/" + url.PathEscape(owner) + "/repos", "/users/" + url.PathEscape(owner) + "/repos"} {
		out = out[:0]
		var failed bool
		for page := 1; page <= 30; page++ {
			var body []repoJSON
			if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=100&page=%d", base, page), c.patAuth(), nil, &body); err != nil {
				failed = true
				break
			}
			for _, r := range body {
				out = append(out, Repo{
					Owner: r.Owner.Login, Name: r.Name, DefaultBranch: r.DefaultBranch,
					CloneURL: r.CloneURL, Private: r.Private,
				})
			}
			if len(body) < 100 {
				break
			}
		}
		if !failed {
			return out, nil
		}
	}
	return nil, &NotInstalledError{Owner: owner, Name: "*", Role: c.Role}
}

// patOwner answers AppOwner for a PAT: the token's own user, which is the account every push
// and every pull request will be attributed to.
func (c *Client) patOwner(ctx context.Context) (login, kind string, err error) {
	var u struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/user", c.patAuth(), nil, &u); err != nil {
		return "", "", err
	}
	kind = "user"
	if u.Type == "Organization" {
		kind = "org"
	}
	return u.Login, kind, nil
}

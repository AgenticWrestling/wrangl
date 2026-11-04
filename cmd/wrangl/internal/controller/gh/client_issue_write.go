package gh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// issueToken mints a token for one repository with only issues: write. A refusal at this point
// (403, or 422 "permissions not granted") means the installation has not accepted issues: write.
func (c *Client) issueToken(ctx context.Context, r Repo) (string, error) {
	tok, _, err := c.mintToken(ctx, r, map[string]string{"issues": "write"})
	if err == nil {
		return tok, nil
	}
	// mintToken folds 4xx into NotInstalledError; for writes the likelier cause is the permission.
	var ni *NotInstalledError
	if errors.As(err, &ni) {
		return "", fmt.Errorf("%w (or the App is not installed on %s/%s)", ErrIssuePermission, r.Owner, r.Name)
	}
	return "", err
}

// issueWrite runs one write; a 403 from GitHub is ErrIssuePermission, 404 is ErrIssueNotFound.
func (c *Client) issueWrite(ctx context.Context, r Repo, method, path string, body, out any) error {
	tok, err := c.issueToken(ctx, r)
	if err != nil {
		return err
	}
	if _, err := c.do(ctx, method, repoPath(r)+path, "token "+tok, body, out); err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			switch he.Status {
			case http.StatusForbidden:
				return fmt.Errorf("%w: %s", ErrIssuePermission, he.Body)
			case http.StatusNotFound:
				return ErrIssueNotFound
			}
		}
		return err
	}
	return nil
}

// CreateIssue implements IssueWriter.
func (c *Client) CreateIssue(ctx context.Context, r Repo, title, body string, labels []string) (Issue, error) {
	req := map[string]any{"title": title, "body": body}
	if len(labels) > 0 {
		req["labels"] = labels
	}
	var w wireIssue
	if err := c.issueWrite(ctx, r, http.MethodPost, "/issues", req, &w); err != nil {
		return Issue{}, err
	}
	return w.issue(), nil
}

// EditIssue implements IssueWriter. GitHub replaces the whole label set on PATCH, so adds and
// removes are applied to the current labels.
func (c *Client) EditIssue(ctx context.Context, r Repo, number int64, e IssueEdit) (Issue, error) {
	cur, err := c.GetIssue(ctx, r, number) // also refuses pull requests, which share the endpoint
	if err != nil {
		return Issue{}, err
	}
	req := map[string]any{}
	if e.Title != nil {
		req["title"] = *e.Title
	}
	if e.Body != nil {
		req["body"] = *e.Body
	}
	if len(e.AddLabels) > 0 || len(e.RemoveLabels) > 0 {
		req["labels"] = ApplyLabels(cur.Labels, e.AddLabels, e.RemoveLabels)
	}
	var w wireIssue
	if err := c.issueWrite(ctx, r, http.MethodPatch, fmt.Sprintf("/issues/%d", number), req, &w); err != nil {
		return Issue{}, err
	}
	if len(w.PullRequest) > 0 {
		return Issue{}, ErrIsPullRequest
	}
	return w.issue(), nil
}

// ApplyLabels returns have plus add minus remove (case-insensitive), without duplicates.
func ApplyLabels(have, add, remove []string) []string {
	out := []string{}
	for _, l := range append(slices.Clone(have), add...) {
		gone := slices.ContainsFunc(remove, func(x string) bool { return strings.EqualFold(x, l) })
		dup := slices.ContainsFunc(out, func(x string) bool { return strings.EqualFold(x, l) })
		if !gone && !dup {
			out = append(out, l)
		}
	}
	return out
}

// CreateComment implements IssueWriter.
func (c *Client) CreateComment(ctx context.Context, r Repo, number int64, body string) (IssueComment, error) {
	if _, err := c.GetIssue(ctx, r, number); err != nil { // never comment on a pull request here
		return IssueComment{}, err
	}
	var w struct {
		ID        int64                  `json:"id"`
		Body      string                 `json:"body"`
		CreatedAt string                 `json:"created_at"`
		HTMLURL   string                 `json:"html_url"`
		User      struct{ Login string } `json:"user"`
	}
	if err := c.issueWrite(ctx, r, http.MethodPost, fmt.Sprintf("/issues/%d/comments", number), map[string]any{"body": body}, &w); err != nil {
		return IssueComment{}, err
	}
	return IssueComment{ID: w.ID, Author: w.User.Login, Body: w.Body, CreatedAt: w.CreatedAt, URL: w.HTMLURL}, nil
}

// CloseIssue implements IssueWriter.
func (c *Client) CloseIssue(ctx context.Context, r Repo, number int64, reason string) (Issue, error) {
	if _, err := c.GetIssue(ctx, r, number); err != nil {
		return Issue{}, err
	}
	req := map[string]any{"state": "closed"}
	if reason != "" {
		req["state_reason"] = reason
	}
	var w wireIssue
	if err := c.issueWrite(ctx, r, http.MethodPatch, fmt.Sprintf("/issues/%d", number), req, &w); err != nil {
		return Issue{}, err
	}
	if len(w.PullRequest) > 0 {
		return Issue{}, ErrIsPullRequest
	}
	return w.issue(), nil
}

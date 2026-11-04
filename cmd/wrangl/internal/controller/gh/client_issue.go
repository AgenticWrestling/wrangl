package gh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// wireIssueMeta is the part of GitHub's issue JSON the read tools need beyond the basics.
type wireIssueMeta struct {
	User      struct{ Login string }  `json:"user"`
	Labels    []struct{ Name string } `json:"labels"`
	CreatedAt string                  `json:"created_at"`
	UpdatedAt string                  `json:"updated_at"`
	Comments  int                     `json:"comments"`
}

func (m wireIssueMeta) fill(i *Issue) {
	i.Author, i.CreatedAt, i.UpdatedAt, i.Comments = m.User.Login, m.CreatedAt, m.UpdatedAt, m.Comments
	for _, l := range m.Labels {
		i.Labels = append(i.Labels, l.Name)
	}
}

type wireIssue struct {
	Number      int64           `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	HTMLURL     string          `json:"html_url"`
	PullRequest json.RawMessage `json:"pull_request"`
	wireIssueMeta
}

func (w wireIssue) issue() Issue {
	i := Issue{Number: w.Number, Title: w.Title, Body: w.Body, State: w.State, URL: w.HTMLURL}
	w.fill(&i)
	return i
}

func notFound(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

// GetIssue implements IssueReader.
func (c *Client) GetIssue(ctx context.Context, r Repo, number int64) (Issue, error) {
	tok, _, err := c.RepoToken(ctx, r)
	if err != nil {
		return Issue{}, err
	}
	var w wireIssue
	if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/issues/%d", repoPath(r), number), "token "+tok, nil, &w); err != nil {
		if notFound(err) {
			return Issue{}, ErrIssueNotFound
		}
		return Issue{}, err
	}
	if len(w.PullRequest) > 0 {
		return Issue{}, ErrIsPullRequest
	}
	return w.issue(), nil
}

// IssueComments implements IssueReader.
func (c *Client) IssueComments(ctx context.Context, r Repo, number int64, page int) ([]IssueComment, bool, error) {
	tok, _, err := c.RepoToken(ctx, r)
	if err != nil {
		return nil, false, err
	}
	if page < 1 {
		page = 1
	}
	var items []struct {
		ID        int64                  `json:"id"`
		Body      string                 `json:"body"`
		CreatedAt string                 `json:"created_at"`
		User      struct{ Login string } `json:"user"`
	}
	h, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/issues/%d/comments?per_page=100&page=%d", repoPath(r), number, page), "token "+tok, nil, &items)
	if err != nil {
		if notFound(err) {
			return nil, false, ErrIssueNotFound
		}
		return nil, false, err
	}
	out := make([]IssueComment, 0, len(items))
	for _, it := range items {
		out = append(out, IssueComment{ID: it.ID, Author: it.User.Login, Body: it.Body, CreatedAt: it.CreatedAt})
	}
	return out, nextLink.MatchString(h.Get("Link")), nil
}

// SearchIssues implements IssueReader using GitHub's search API, restricted to this repository
// and to issues (never pull requests).
func (c *Client) SearchIssues(ctx context.Context, r Repo, query, state string, limit int) ([]Issue, error) {
	tok, _, err := c.RepoToken(ctx, r)
	if err != nil {
		return nil, err
	}
	if limit < 1 || limit > 100 {
		limit = 30
	}
	q := fmt.Sprintf("repo:%s/%s is:issue %s", r.Owner, r.Name, strings.TrimSpace(query))
	if state == "open" || state == "closed" {
		q += " is:" + state
	}
	var res struct {
		Items []wireIssue `json:"items"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/search/issues?per_page="+fmt.Sprint(limit)+"&q="+url.QueryEscape(q), "token "+tok, nil, &res); err != nil {
		return nil, err
	}
	var out []Issue
	for _, w := range res.Items {
		if len(w.PullRequest) == 0 {
			out = append(out, w.issue())
		}
	}
	return out, nil
}

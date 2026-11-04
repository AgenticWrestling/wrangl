package gh

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var instCache sync.Map // "<appID>/<owner>/<name>" -> int64

// tokenFor mints an installation token for this App's installation on r (each App has
// its own installation, so the author's id stored with the repo is not reused).
func (c *Client) tokenFor(ctx context.Context, r Repo) (string, error) {
	key := fmt.Sprintf("%d/%s/%s", c.AppID, r.Owner, r.Name)
	id, ok := instCache.Load(key)
	if !ok {
		auth, err := c.appAuth()
		if err != nil {
			return "", err
		}
		var inst struct {
			ID int64 `json:"id"`
		}
		if _, err := c.do(ctx, http.MethodGet, "/repos/"+url.PathEscape(r.Owner)+"/"+url.PathEscape(r.Name)+"/installation", auth, nil, &inst); err != nil {
			return "", fmt.Errorf("app %d on %s/%s: %w", c.AppID, r.Owner, r.Name, err)
		}
		id = inst.ID
		instCache.Store(key, id)
	}
	rr := r
	rr.InstallationID = id.(int64)
	tok, _, err := c.RepoToken(ctx, rr)
	return tok, err
}

func repoPath(r Repo) string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

type ghPR struct {
	Number  int64  `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	Head    struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

func (p ghPR) pr() PR {
	return PR{Number: p.Number, URL: p.HTMLURL, HeadSHA: p.Head.SHA, State: p.State, Merged: p.Merged}
}

// CreatePR implements PRAPI.
func (c *Client) CreatePR(ctx context.Context, r Repo, head, base, title, body string) (PR, error) {
	tok, err := c.tokenFor(ctx, r)
	if err != nil {
		return PR{}, err
	}
	var out ghPR
	_, err = c.do(ctx, http.MethodPost, repoPath(r)+"/pulls", "token "+tok,
		map[string]any{"head": head, "base": base, "title": title, "body": body}, &out)
	return out.pr(), err
}

// GetPR implements PRAPI.
func (c *Client) GetPR(ctx context.Context, r Repo, n int64) (PR, error) {
	tok, err := c.tokenFor(ctx, r)
	if err != nil {
		return PR{}, err
	}
	var out ghPR
	_, err = c.do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d", repoPath(r), n), "token "+tok, nil, &out)
	return out.pr(), err
}

// PRFiles implements PRAPI.
func (c *Client) PRFiles(ctx context.Context, r Repo, n int64) ([]PRFile, error) {
	tok, err := c.tokenFor(ctx, r)
	if err != nil {
		return nil, err
	}
	var all []PRFile
	for page := 1; page <= 30; page++ {
		var items []struct {
			Filename  string `json:"filename"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
		}
		if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d/files?per_page=100&page=%d", repoPath(r), n, page), "token "+tok, nil, &items); err != nil {
			return nil, err
		}
		for _, it := range items {
			all = append(all, PRFile{Path: it.Filename, Additions: it.Additions, Deletions: it.Deletions})
		}
		if len(items) < 100 {
			break
		}
	}
	return all, nil
}

// PRDiff implements PRAPI.
func (c *Client) PRDiff(ctx context.Context, r Repo, n int64) (string, error) {
	tok, err := c.tokenFor(ctx, r)
	if err != nil {
		return "", err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s%s/pulls/%d", c.base(), repoPath(r), n), nil)
	req.Header.Set("Accept", "application/vnd.github.diff")
	req.Header.Set("Authorization", "token "+tok)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return "", &HTTPError{Status: resp.StatusCode, Body: string(b)}
	}
	return string(b), err
}

// Checks implements PRAPI: check runs and commit statuses combined.
func (c *Client) Checks(ctx context.Context, r Repo, sha string) (string, error) {
	tok, err := c.tokenFor(ctx, r)
	if err != nil {
		return "", err
	}
	var runs struct {
		Total int `json:"total_count"`
		Runs  []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/commits/%s/check-runs?per_page=100", repoPath(r), url.PathEscape(sha)), "token "+tok, nil, &runs); err != nil {
		return "", err
	}
	var status struct {
		State    string `json:"state"`
		Statuses []any  `json:"statuses"`
	}
	if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/commits/%s/status", repoPath(r), url.PathEscape(sha)), "token "+tok, nil, &status); err != nil {
		return "", err
	}
	if runs.Total == 0 && len(status.Statuses) == 0 {
		return ChecksNone, nil
	}
	result := ChecksSuccess
	for _, cr := range runs.Runs {
		switch {
		case cr.Status != "completed":
			result = ChecksPending
		case cr.Conclusion != "success" && cr.Conclusion != "neutral" && cr.Conclusion != "skipped":
			return ChecksFailure, nil
		}
	}
	if len(status.Statuses) > 0 {
		switch status.State {
		case "failure", "error":
			return ChecksFailure, nil
		case "pending":
			result = ChecksPending
		}
	}
	return result, nil
}

// Review implements PRAPI.
func (c *Client) Review(ctx context.Context, r Repo, n int64, event, body string) error {
	tok, err := c.tokenFor(ctx, r)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPost, fmt.Sprintf("%s/pulls/%d/reviews", repoPath(r), n), "token "+tok,
		map[string]string{"event": event, "body": body}, nil)
	return err
}

// Merge implements PRAPI (squash).
func (c *Client) Merge(ctx context.Context, r Repo, n int64, title string) error {
	tok, err := c.tokenFor(ctx, r)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPut, fmt.Sprintf("%s/pulls/%d/merge", repoPath(r), n), "token "+tok,
		map[string]string{"merge_method": "squash", "commit_title": title}, nil)
	return err
}

// ClosePR implements PRAPI.
func (c *Client) ClosePR(ctx context.Context, r Repo, n int64) error {
	tok, err := c.tokenFor(ctx, r)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", repoPath(r), n), "token "+tok, map[string]string{"state": "closed"}, nil)
	return err
}

// OpenPRs implements PRAPI.
func (c *Client) OpenPRs(ctx context.Context, r Repo) ([]OpenPR, error) {
	tok, err := c.tokenFor(ctx, r)
	if err != nil {
		return nil, err
	}
	var out []OpenPR
	for page := 1; page <= 5; page++ {
		var raw []struct {
			ghPR
			Title string `json:"title"`
			Head  struct {
				SHA  string `json:"sha"`
				Ref  string `json:"ref"`
				Repo *struct {
					FullName string `json:"full_name"`
				} `json:"repo"`
			} `json:"head"`
		}
		if _, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls?state=open&per_page=100&page=%d", repoPath(r), page), "token "+tok, nil, &raw); err != nil {
			return out, err
		}
		for _, p := range raw {
			pr := p.pr()
			pr.HeadSHA = p.Head.SHA
			out = append(out, OpenPR{PR: pr, Title: p.Title, HeadBranch: p.Head.Ref,
				Fork: p.Head.Repo == nil || !strings.EqualFold(p.Head.Repo.FullName, r.Owner+"/"+r.Name)})
		}
		if len(raw) < 100 {
			break
		}
	}
	return out, nil
}

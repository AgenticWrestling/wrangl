package gh

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Client is a GitHub App client (RS256 app JWT -> installation tokens).
type Client struct {
	AppID   int64
	Key     *rsa.PrivateKey
	BaseURL string // default https://api.github.com
	HTTP    *http.Client
	Now     func() time.Time
	Role    string // "" (author) or "reviewer": named in errors
}

// LoadKey reads a GitHub App private key (PKCS#1 or PKCS#8 PEM).
func LoadKey(path string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an RSA key", path)
	}
	return rk, nil
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "https://api.github.com"
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// AppJWT signs the short-lived JWT that authenticates as the App itself.
func (c *Client) AppJWT() (string, error) {
	enc := base64.RawURLEncoding
	now := c.now().Unix()
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now - 60, "exp": now + 9*60, "iss": fmt.Sprint(c.AppID)})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// HTTPError is a non-2xx GitHub response.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("github: HTTP %d: %s", e.Status, e.Body) }

func (c *Client) do(ctx context.Context, method, path, auth string, body any, out any) (http.Header, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	u := path
	if !strings.HasPrefix(path, "http") {
		u = c.base() + path
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", auth)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.Header, &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.Header, err
		}
	}
	return resp.Header, nil
}

func (c *Client) appAuth() (string, error) {
	j, err := c.AppJWT()
	return "Bearer " + j, err
}

// Repo implements API.
func (c *Client) Repo(ctx context.Context, owner, name string) (Repo, error) {
	auth, err := c.appAuth()
	if err != nil {
		return Repo{}, err
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	if _, err := c.do(ctx, http.MethodGet, path+"/installation", auth, nil, &inst); err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Status == http.StatusNotFound {
			return Repo{}, &NotInstalledError{Owner: owner, Name: name, Role: c.Role}
		}
		return Repo{}, err
	}
	r := Repo{Owner: owner, Name: name, InstallationID: inst.ID}
	tok, _, err := c.RepoToken(ctx, r)
	if err != nil {
		return Repo{}, err
	}
	var info struct {
		DefaultBranch string `json:"default_branch"`
		CloneURL      string `json:"clone_url"`
		Private       bool   `json:"private"`
	}
	if _, err := c.do(ctx, http.MethodGet, path, "token "+tok, nil, &info); err != nil {
		return Repo{}, err
	}
	r.DefaultBranch, r.CloneURL, r.Private = info.DefaultBranch, info.CloneURL, info.Private
	return r, nil
}

// RepoToken implements API.
func (c *Client) RepoToken(ctx context.Context, r Repo) (string, time.Time, error) {
	auth, err := c.appAuth()
	if err != nil {
		return "", time.Time{}, err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	body := map[string]any{
		"repositories": []string{r.Name},
		"permissions":  map[string]string{"contents": "write", "pull_requests": "write"},
	}
	if _, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", r.InstallationID), auth, body, &out); err != nil {
		// 404/401/403/422 mean the installation is gone or no longer covers this repository (it was
		// uninstalled, or the repo moved to another owner): the owner must (re)install the App.
		var he *HTTPError
		if errors.As(err, &he) && (he.Status == 404 || he.Status == 401 || he.Status == 403 || he.Status == 422) {
			return "", time.Time{}, &NotInstalledError{Owner: r.Owner, Name: r.Name}
		}
		return "", time.Time{}, err
	}
	return out.Token, out.ExpiresAt, nil
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// Issues implements API (follows Link pagination; pull requests are skipped).
func (c *Client) Issues(ctx context.Context, r Repo, state string) ([]Issue, error) {
	tok, _, err := c.RepoToken(ctx, r)
	if err != nil {
		return nil, err
	}
	next := fmt.Sprintf("/repos/%s/%s/issues?state=%s&per_page=100", url.PathEscape(r.Owner), url.PathEscape(r.Name), url.QueryEscape(state))
	var out []Issue
	for page := 0; next != "" && page < 50; page++ {
		var items []struct {
			Number      int64           `json:"number"`
			Title       string          `json:"title"`
			Body        string          `json:"body"`
			State       string          `json:"state"`
			HTMLURL     string          `json:"html_url"`
			PullRequest json.RawMessage `json:"pull_request"`
			wireIssueMeta
		}
		h, err := c.do(ctx, http.MethodGet, next, "token "+tok, nil, &items)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if len(it.PullRequest) > 0 {
				continue
			}
			i := Issue{Number: it.Number, Title: it.Title, Body: it.Body, State: it.State, URL: it.HTMLURL}
			it.wireIssueMeta.fill(&i)
			out = append(out, i)
		}
		next = ""
		if m := nextLink.FindStringSubmatch(h.Get("Link")); m != nil {
			next = m[1]
		}
	}
	return out, nil
}

// AppOwner returns the login and kind ("user" or "org") of the account that owns this App.
func (c *Client) AppOwner(ctx context.Context) (login, kind string, err error) {
	auth, err := c.appAuth()
	if err != nil {
		return "", "", err
	}
	var app struct {
		Slug  string `json:"slug"`
		Owner struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"owner"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/app", auth, nil, &app); err != nil {
		return "", "", err
	}
	kind = "user"
	if app.Owner.Type == "Organization" {
		kind = "org"
	}
	return app.Owner.Login, kind, nil
}

// ListRepos implements API: the repositories the App can see in owner's installation.
func (c *Client) ListRepos(ctx context.Context, owner string) ([]Repo, error) {
	auth, err := c.appAuth()
	if err != nil {
		return nil, err
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	_, err = c.do(ctx, http.MethodGet, "/users/"+url.PathEscape(owner)+"/installation", auth, nil, &inst)
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusNotFound {
		_, err = c.do(ctx, http.MethodGet, "/orgs/"+url.PathEscape(owner)+"/installation", auth, nil, &inst)
	}
	if errors.As(err, &he) && he.Status == http.StatusNotFound {
		return nil, &NotInstalledError{Owner: owner, Name: "*", Role: c.Role}
	} else if err != nil {
		return nil, err
	}
	var tok struct {
		Token string `json:"token"`
	}
	if _, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID), auth, nil, &tok); err != nil {
		return nil, err
	}
	var out []Repo
	next := "/installation/repositories?per_page=100"
	for page := 0; next != "" && page < 30; page++ {
		var body struct {
			Repositories []struct {
				Name          string `json:"name"`
				DefaultBranch string `json:"default_branch"`
				CloneURL      string `json:"clone_url"`
				Private       bool   `json:"private"`
				Owner         struct {
					Login string `json:"login"`
				} `json:"owner"`
			} `json:"repositories"`
		}
		h, err := c.do(ctx, http.MethodGet, next, "token "+tok.Token, nil, &body)
		if err != nil {
			return nil, err
		}
		for _, r := range body.Repositories {
			out = append(out, Repo{Owner: r.Owner.Login, Name: r.Name, DefaultBranch: r.DefaultBranch, CloneURL: r.CloneURL, InstallationID: inst.ID, Private: r.Private})
		}
		next = ""
		if m := nextLink.FindStringSubmatch(h.Get("Link")); m != nil {
			next = m[1]
		}
	}
	return out, nil
}

// Package client is the typed HTTP client for the controller API, used by the CLI and TUI.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nreid/devhost/cmd/wrangl/internal/config"
	"github.com/nreid/devhost/cmd/wrangl/internal/proto"
)

// Client talks to one controller over http(s):// or unix:///path.
type Client struct {
	base string
	hc   *http.Client
	cf   *config.CFServiceToken
}

// New builds a client. cf may be nil.
func New(controller string, cf *config.CFServiceToken) (*Client, error) {
	u, err := url.Parse(controller)
	if err != nil {
		return nil, fmt.Errorf("controller URL %q: %w", controller, err)
	}
	c := &Client{cf: cf, hc: &http.Client{Timeout: 60 * time.Second}}
	switch u.Scheme {
	case "http", "https":
		c.base = strings.TrimRight(controller, "/")
	case "unix":
		sock := u.Path
		c.base = "http://wrangl"
		c.hc.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}
	default:
		return nil, fmt.Errorf("controller URL %q: want http(s):// or unix:///path", controller)
	}
	return c, nil
}

// Error is a non-2xx API response.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("controller: %s (HTTP %d)", e.Message, e.Status) }

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.cf != nil {
		req.Header.Set("CF-Access-Client-Id", c.cf.ID)
		req.Header.Set("CF-Access-Client-Secret", c.cf.Secret)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("controller unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e proto.APIError
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return &Error{Status: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) json(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	return c.do(ctx, method, path, body, "application/json", out)
}

// Runners lists runners; all includes terminated ones.
func (c *Client) Runners(ctx context.Context, all bool) ([]proto.RunnerView, error) {
	var out []proto.RunnerView
	q := ""
	if all {
		q = "?all=1"
	}
	return out, c.json(ctx, http.MethodGet, "/api/v1/runners"+q, nil, &out)
}

// Runner returns one runner by name.
func (c *Client) Runner(ctx context.Context, name string) (proto.RunnerView, error) {
	var out proto.RunnerView
	return out, c.json(ctx, http.MethodGet, "/api/v1/runners/"+url.PathEscape(name), nil, &out)
}

// Launch starts count runners from template.
func (c *Client) Launch(ctx context.Context, template string, count int) ([]proto.RunnerView, error) {
	var out []proto.RunnerView
	return out, c.json(ctx, http.MethodPost, "/api/v1/runners", proto.LaunchRequest{Template: template, Count: count}, &out)
}

// Terminate stops a controller-launched runner.
func (c *Client) Terminate(ctx context.Context, name string) error {
	return c.json(ctx, http.MethodDelete, "/api/v1/runners/"+url.PathEscape(name), nil, nil)
}

// EnrollToken mints a one-time token for adopting a box.
func (c *Client) EnrollToken(ctx context.Context, template string, ttl time.Duration) (proto.EnrollTokenResponse, error) {
	var out proto.EnrollTokenResponse
	return out, c.json(ctx, http.MethodPost, "/api/v1/enroll-tokens", proto.EnrollTokenRequest{Template: template, TTLS: int64(ttl / time.Second)}, &out)
}

// Templates lists templates.
func (c *Client) Templates(ctx context.Context) ([]proto.TemplateView, error) {
	var out []proto.TemplateView
	return out, c.json(ctx, http.MethodGet, "/api/v1/templates", nil, &out)
}

// Template returns one template.
func (c *Client) Template(ctx context.Context, name string) (proto.TemplateView, error) {
	var out proto.TemplateView
	return out, c.json(ctx, http.MethodGet, "/api/v1/templates/"+url.PathEscape(name), nil, &out)
}

// CreateTemplate uploads a template YAML file.
func (c *Client) CreateTemplate(ctx context.Context, yamlBody []byte) (proto.TemplateView, error) {
	var out proto.TemplateView
	return out, c.do(ctx, http.MethodPost, "/api/v1/templates", bytes.NewReader(yamlBody), "application/yaml", &out)
}

// DeleteTemplate removes an unused template.
func (c *Client) DeleteTemplate(ctx context.Context, name string) error {
	return c.json(ctx, http.MethodDelete, "/api/v1/templates/"+url.PathEscape(name), nil, nil)
}

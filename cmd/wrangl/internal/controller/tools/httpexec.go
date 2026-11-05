package tools

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/toolkdl"
)

// HTTPExecutor runs http tools from the controller (ADR 0003). The URL's scheme and host are
// fixed by the owner's definition; arguments only fill escaped path and query values and JSON
// string values. Every connection goes to an address checked after DNS resolution.
type HTTPExecutor struct {
	// Test-only hooks, unexported so production code cannot set them.
	allowLoopback bool // permit 127.0.0.0/8 and ::1 (a fake upstream on httptest)
	lookup        func(ctx context.Context, host string) ([]netip.Addr, error)
	dial          func(ctx context.Context, network, addr string) (net.Conn, error)
	insecureTLS   bool
}

// NewHTTPExecutor returns the production executor.
func NewHTTPExecutor() *HTTPExecutor { return &HTTPExecutor{} }

const (
	maxRedirects = 3
	userAgent    = "wrangl-tool/1"
	errBodyBytes = 512
)

// deniedPrefixes are never dialed: loopback, private, link-local (which holds the cloud metadata
// address 169.254.169.254), carrier-grade NAT, and other special-purpose ranges. fd00:ec2::254
// (AWS IPv6 metadata) is inside fc00::/7.
var deniedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "64:ff9b:1::/48", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// blocked reports whether ip may not be dialed. IPv4 inside IPv6 forms (mapped, NAT64 and 6to4)
// are judged by the embedded address.
func (x *HTTPExecutor) blocked(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is6() {
		b := ip.As16()
		switch {
		case b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b && allZero(b[4:12]): // 64:ff9b::/96 NAT64
			return x.blocked(netip.AddrFrom4([4]byte(b[12:16])))
		case b[0] == 0x20 && b[1] == 0x02: // 2002::/16 6to4
			return x.blocked(netip.AddrFrom4([4]byte(b[2:6])))
		}
	}
	if x.allowLoopback && ip.IsLoopback() {
		return false
	}
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	for _, p := range deniedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// dialContext resolves the host, refuses the call if ANY address is blocked (a mixed answer is
// how rebinding attacks hide a private address), and dials the checked IP itself, so the
// connection cannot be steered by a second DNS answer.
func (x *HTTPExecutor) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		ips = []netip.Addr{ip}
	} else if x.lookup != nil {
		if ips, err = x.lookup(ctx, host); err != nil {
			return nil, fmt.Errorf("resolve %s: %w", host, err)
		}
	} else {
		as, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", host, err)
		}
		ips = as
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, ip := range ips {
		if x.blocked(ip) {
			return nil, fmt.Errorf("refusing to connect to %s: it resolves to a private, loopback, link-local or metadata address", host)
		}
	}
	dial := x.dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	}
	var last error
	for _, ip := range ips {
		c, err := dial(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

// Execute implements Executor.
func (x *HTTPExecutor) Execute(ctx context.Context, c *Call) ([]byte, error) {
	h := c.Def.HTTP
	if h == nil {
		return nil, errors.New("not an http tool")
	}
	u, err := buildURL(h.URL, c.Input)
	if err != nil {
		return nil, err
	}
	body, ctype, err := buildBody(h, c)
	if err != nil {
		return nil, err
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, h.Method, u.String(), rd)
	if err != nil {
		return nil, errors.New("could not build the request")
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain;q=0.9, */*;q=0.1")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for _, hd := range h.Headers { // only the headers the definition declares
		v, err := c.Secrets.Expand(hd.Value)
		if err != nil {
			return nil, err
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			return nil, fmt.Errorf("header %s: the resolved value contains control characters", hd.Name)
		}
		req.Header.Set(hd.Name, v)
	}

	origin := hostPort(u)
	client := &http.Client{
		Timeout: time.Duration(h.TimeoutS) * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil, // an environment proxy would bypass the address checks
			DialContext:           x.dialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: x.insecureTLS},
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: time.Duration(h.TimeoutS) * time.Second,
			DisableKeepAlives:     true,
			ForceAttemptHTTP2:     true,
		},
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			if r.URL.Scheme != "https" || hostPort(r.URL) != origin {
				return fmt.Errorf("refusing a redirect to a different host (%s); tools may only follow redirects on the same host", r.URL.Hostname())
			}
			return nil // the new connection is resolved and checked like the first
		},
	}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", h.Method, u.Hostname(), unwrapURLError(err))
	}
	defer resp.Body.Close()
	// Read past the cap by a secret's length so scrubbing sees whole values before the engine cuts.
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(c.Def.OutputLimit())+64<<10))
	if err != nil {
		return nil, fmt.Errorf("reading the response: %w", unwrapURLError(err))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if len(data) > errBodyBytes {
			data = data[:errBodyBytes]
		}
		return nil, fmt.Errorf("upstream returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err // drops the URL (and so any query values) from the message
	}
	return err
}

func hostPort(u *url.URL) string {
	p := u.Port()
	if p == "" {
		p = "443"
	}
	return strings.ToLower(u.Hostname()) + ":" + p
}

// buildURL fills the placeholders after the host. The authority is the literal text of the
// definition, and values are escaped for the position they land in, so an argument can add
// neither a segment, a parameter nor a different host.
func buildURL(tmpl string, in *toolkdl.Input) (*url.URL, error) {
	const pre = "https://"
	rest := strings.TrimPrefix(tmpl, pre)
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	authority, tail := rest[:end], rest[end:]
	inQuery := false
	var b strings.Builder
	var verr error
	last := 0
	for _, loc := range placeholderIdx.FindAllStringSubmatchIndex(tail, -1) {
		b.WriteString(tail[last:loc[0]])
		last = loc[1]
		if i := strings.IndexByte(tail[:loc[0]], '?'); i >= 0 {
			inQuery = true
		}
		name := tail[loc[4]:loc[5]]
		v, ok := in.Args[name]
		if !ok { // an optional argument that was not given
			v = ""
		}
		if inQuery {
			b.WriteString(url.QueryEscape(v))
			continue
		}
		if v == "." || v == ".." {
			verr = fmt.Errorf("argument %q cannot be a path dot segment", name)
		}
		b.WriteString(url.PathEscape(v))
	}
	b.WriteString(tail[last:])
	if verr != nil {
		return nil, verr
	}
	u, err := url.Parse(pre + authority + b.String())
	if err != nil || u.Scheme != "https" || u.User != nil || !strings.EqualFold(u.Host, authority) {
		return nil, errors.New("the request URL is not valid")
	}
	return u, nil
}

var (
	placeholderAny   = regexp.MustCompile(`\{\{\s*(input|secret)\.([A-Za-z0-9_.-]*)\s*\}\}`)
	placeholderIdx   = placeholderAny
	wholePlaceholder = regexp.MustCompile(`^\{\{\s*(input|secret)\.([A-Za-z0-9_.-]*)\s*\}\}$`)
)

// buildBody renders the body. A body template that is JSON is parsed first and values are put
// into string positions of the decoded tree, so a value can never close a string or add a key;
// {{input.x}} standing alone as a string takes the argument's own JSON type. Other templates are
// text: values are inserted verbatim, or form-encoded when the Content-Type header says so.
func buildBody(h *toolkdl.HTTP, c *Call) ([]byte, string, error) {
	if h.Body == "" {
		return nil, "", nil
	}
	declared := ""
	for _, hd := range h.Headers {
		if strings.EqualFold(hd.Name, "content-type") {
			declared = hd.Value
		}
	}
	trimmed := strings.TrimSpace(h.Body)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		var tree any
		if err := dec.Decode(&tree); err != nil || dec.More() {
			return nil, "", errors.New("the body template is not valid JSON (placeholders must be inside string values)")
		}
		out, err := fillJSON(tree, c)
		if err != nil {
			return nil, "", err
		}
		b, err := json.Marshal(out)
		if err != nil {
			return nil, "", errors.New("could not encode the JSON body")
		}
		ct := ""
		if declared == "" {
			ct = "application/json"
		}
		return b, ct, nil
	}
	form := strings.Contains(strings.ToLower(declared), "application/x-www-form-urlencoded")
	var verr error
	text := placeholderAny.ReplaceAllStringFunc(h.Body, func(m string) string {
		sub := placeholderAny.FindStringSubmatch(m)
		var v string
		if sub[1] == "input" {
			v = c.Input.Args[sub[2]]
		} else {
			s, err := c.Secrets.Expand(m)
			if err != nil {
				verr = err
			}
			v = s
		}
		if form {
			return url.QueryEscape(v)
		}
		return v
	})
	if verr != nil {
		return nil, "", verr
	}
	ct := ""
	if declared == "" {
		ct = "text/plain; charset=utf-8"
	}
	return []byte(text), ct, nil
}

type omit struct{}

var omitted any = omit{}

func fillJSON(v any, c *Call) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			nk, err := fillString(k, c)
			if err != nil {
				return nil, err
			}
			nv, err := fillJSON(x, c)
			if err != nil {
				return nil, err
			}
			if nv == omitted {
				continue
			}
			out[nk] = nv
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(t))
		for _, x := range t {
			nv, err := fillJSON(x, c)
			if err != nil {
				return nil, err
			}
			if nv != omitted {
				out = append(out, nv)
			}
		}
		return out, nil
	case string:
		if m := wholePlaceholder.FindStringSubmatch(t); m != nil && m[1] == "input" {
			if val, ok := c.Input.Values[m[2]]; ok {
				return val, nil // typed: an integer stays a number
			}
			return omitted, nil // optional and not given: the key is left out
		}
		return fillString(t, c)
	}
	return v, nil
}

func fillString(s string, c *Call) (string, error) {
	var verr error
	out := placeholderAny.ReplaceAllStringFunc(s, func(m string) string {
		sub := placeholderAny.FindStringSubmatch(m)
		if sub[1] == "input" {
			return c.Input.Args[sub[2]]
		}
		v, err := c.Secrets.Expand(m)
		if err != nil {
			verr = err
		}
		return v
	})
	return out, verr
}

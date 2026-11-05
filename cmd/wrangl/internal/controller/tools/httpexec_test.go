package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/toolkdl"
)

const secretToken = "tok-LIVE-7f3a9c"

type seen struct {
	Method, Path, RawQuery, Auth, CType, Body string
	Query                                     url.Values
}

// upstream is a fake HTTPS API on loopback, reached through a fake hostname.
type upstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []seen
	h    http.HandlerFunc
	port string
}

func newUpstream(t *testing.T, h http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{h: h}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, seen{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(b), r.URL.Query()})
		u.mu.Unlock()
		u.h(w, r)
	}))
	t.Cleanup(u.srv.Close)
	_, u.port, _ = net.SplitHostPort(u.srv.Listener.Addr().String())
	return u
}

func (u *upstream) last() seen {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		return seen{}
	}
	return u.reqs[len(u.reqs)-1]
}

func (u *upstream) count() int { u.mu.Lock(); defer u.mu.Unlock(); return len(u.reqs) }

// testExec resolves the fake hostnames to loopback and permits loopback, the test-only hook.
func testExec() *HTTPExecutor {
	return &HTTPExecutor{allowLoopback: true, insecureTLS: true,
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			if strings.HasSuffix(host, ".example.test") {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return nil, errors.New("no such host")
		}}
}

func httpTool(name, port, extra string) string {
	return fmt.Sprintf(`tool %q {
  description "fake api"
  effect "read"
  run-kinds "work"
  input {
    param "id" type="string" { max-length 40 }
    param "q" type="string" required=#false { max-length 80 }
    param "n" type="integer" required=#false { min 1; max 99 }
  }
  http {
    method "GET"
    url "https://api.example.test:%s/v1/items/{{input.id}}?q={{input.q}}"
    header "Authorization" "Bearer {{secret.api.token}}"
    timeout 3
    max-output-kb 1
    %s
  }
}`, name, port, extra)
}

func httpEnv(t *testing.T, h http.HandlerFunc, src func(port string) string) (*env, *upstream) {
	t.Helper()
	v := newEnv(t)
	up := newUpstream(t, h)
	v.e.Executors[toolkdl.KindHTTP] = testExec()
	if _, err := v.svc.Add(context.Background(), src(up.port)); err != nil {
		t.Fatal(err)
	}
	return v, up
}

func enableAndKey(t *testing.T, v *env, name string) {
	t.Helper()
	ctx := context.Background()
	if err := v.svc.Enable(ctx, "alpha", name); err != nil {
		t.Fatal(err)
	}
	if err := v.svc.SetSecret(ctx, "alpha", "api.token", strings.NewReader(secretToken)); err != nil {
		t.Fatal(err)
	}
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, `{"ok":true,"saw_auth":%q}`, r.Header.Get("Authorization"))
}

func TestHTTPCallEndToEnd(t *testing.T) {
	v, up := httpEnv(t, okHandler, func(p string) string { return httpTool("lookup", p, "") })
	enableAndKey(t, v, "lookup")
	out, err := v.call("work", "lookup", `{"id":"abc-1","q":"hello world"}`)
	if err != nil {
		t.Fatal(err)
	}
	got := up.last()
	if got.Method != "GET" || got.Path != "/v1/items/abc-1" || got.Query.Get("q") != "hello world" || got.Auth != "Bearer "+secretToken {
		t.Fatalf("upstream saw %+v", got)
	}
	text := out.(map[string]any)["output"].(string)
	if strings.Contains(text, secretToken) || !strings.Contains(text, Redacted) {
		t.Fatalf("the echoed token must be scrubbed from the output: %s", text)
	}
	if rows := v.audit(t); len(rows) != 1 || rows[0] != "ext_lookup:ok" {
		t.Fatalf("audit: %v", rows)
	}
}

func TestHTTPArgumentsCannotChangeThePath_QueryOrHost(t *testing.T) {
	v, up := httpEnv(t, okHandler, func(p string) string { return httpTool("lookup", p, "") })
	enableAndKey(t, v, "lookup")
	cases := []struct {
		name, args, wantPath string
		wantQuery            func(url.Values) bool
	}{
		{"slash", `{"id":"a/b"}`, "/v1/items/a%2Fb", nil},
		{"percent", `{"id":"a%2e%2e"}`, "/v1/items/a%252e%252e", nil},
		{"query smuggling", `{"id":"x","q":"1&admin=true#frag"}`, "/v1/items/x", func(q url.Values) bool { return len(q) == 1 && q.Get("q") == "1&admin=true#frag" }},
		{"newline", `{"id":"x","q":"a\r\nHost: evil"}`, "/v1/items/x", func(q url.Values) bool { return q.Get("q") == "a\r\nHost: evil" }},
		{"at sign", `{"id":"@evil.example.test"}`, "/v1/items/@evil.example.test", nil},
	}
	for _, c := range cases {
		if _, err := v.call("work", "lookup", c.args); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		got := up.last()
		if got.Path != c.wantPath || (c.wantQuery != nil && !c.wantQuery(got.Query)) {
			t.Errorf("%s: upstream saw path=%s query=%s", c.name, got.Path, got.RawQuery)
		}
	}
	// dot segments cannot climb out of the fixed prefix
	for _, id := range []string{"..", "."} {
		before := up.count()
		_, err := v.call("work", "lookup", fmt.Sprintf(`{"id":%q}`, id))
		if err == nil || !strings.Contains(err.Error(), "dot segment") || up.count() != before {
			t.Errorf("%q: %v", id, err)
		}
	}
}

func TestHTTPOnlyDeclaredHeadersAndSecretsInHeaders(t *testing.T) {
	v, up := httpEnv(t, okHandler, func(p string) string { return httpTool("lookup", p, `header "X-Env" "prod"`) })
	enableAndKey(t, v, "lookup")
	if _, err := v.call("work", "lookup", `{"id":"1"}`); err != nil {
		t.Fatal(err)
	}
	// input cannot add headers: unknown arguments are refused before any request
	n := up.count()
	if _, err := v.call("work", "lookup", `{"id":"1","header":"X-Evil: 1"}`); !errors.Is(err, errs.Invalid) || up.count() != n {
		t.Fatalf("extra argument: %v", err)
	}
	// a stored secret containing a newline cannot split the header
	v.svc.SetSecret(context.Background(), "alpha", "api.token", strings.NewReader("abc\r\nX-Injected: 1"))
	// SetSecret keeps interior newlines, so the executor must refuse it
	if _, err := v.call("work", "lookup", `{"id":"1"}`); err == nil || !strings.Contains(err.Error(), "control characters") || up.count() != n {
		t.Fatalf("header splitting: %v", err)
	}
}

func jsonTool(p string) string {
	return fmt.Sprintf(`tool "create" {
  description "create"
  effect "write"
  run-kinds "work"
  input {
    param "title" type="string" { max-length 100 }
    param "count" type="integer" { min 1; max 50 }
    param "note" type="string" required=#false { max-length 50 }
  }
  http {
    method "POST"
    url "https://api.example.test:%s/v1/things"
    header "Authorization" "Bearer {{secret.api.token}}"
    body "{\"title\": \"{{input.title}}\", \"count\": \"{{input.count}}\", \"note\": \"{{input.note}}\", \"meta\": {\"by\": \"wrangl {{input.title}}\", \"key\": \"{{secret.api.token}}\"}}"
  }
}`, p)
}

func TestHTTPJSONBodyIsBuiltNotConcatenated(t *testing.T) {
	v, up := httpEnv(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("created")) }, jsonTool)
	enableAndKey(t, v, "create")
	evil := `x","admin":true,"y":"` + "\n\\"
	args, _ := json.Marshal(map[string]any{"title": evil, "count": 7})
	if _, err := v.call("work", "create", string(args)); err != nil {
		t.Fatal(err)
	}
	got := up.last()
	if got.Method != "POST" || got.CType != "application/json" {
		t.Fatalf("request: %+v", got)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(got.Body), &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, got.Body)
	}
	if body["title"] != evil || body["count"] != float64(7) {
		t.Fatalf("title/count not typed or intact: %v", body)
	}
	if _, injected := body["admin"]; injected || len(body) != 3 && len(body) != 4 {
		t.Fatalf("injected keys: %v", body)
	}
	if _, has := body["note"]; has {
		t.Fatalf("an optional argument that was not given must leave its key out: %v", body)
	}
	meta := body["meta"].(map[string]any)
	if meta["by"] != "wrangl "+evil || meta["key"] != secretToken {
		t.Fatalf("meta: %v", meta)
	}
}

func TestHTTPBodyTemplateMustBeJSON(t *testing.T) {
	v, _ := httpEnv(t, okHandler, func(p string) string {
		return strings.Replace(jsonTool(p), `"title\": \"{{input.title}}\",`, `"title\": {{input.count}},`, 1)
	})
	enableAndKey(t, v, "create")
	if _, err := v.call("work", "create", `{"title":"t","count":3}`); err == nil || !strings.Contains(err.Error(), "valid JSON") {
		t.Fatalf("placeholder outside a string: %v", err)
	}
}

func TestHTTPFormBodyIsEncoded(t *testing.T) {
	v, up := httpEnv(t, okHandler, func(p string) string {
		return fmt.Sprintf(`tool "form" {
  description "form"
  effect "write"
  run-kinds "work"
  input { param "name" type="string" { max-length 50 } }
  http {
    method "POST"
    url "https://api.example.test:%s/f"
    header "Content-Type" "application/x-www-form-urlencoded"
    header "Authorization" "Bearer {{secret.api.token}}"
    body "name={{input.name}}&kind=fixed"
  }
}`, p)
	})
	enableAndKey(t, v, "form")
	if _, err := v.call("work", "form", `{"name":"a&kind=evil"}`); err != nil {
		t.Fatal(err)
	}
	if got := up.last(); got.Body != "name=a%26kind%3Devil&kind=fixed" {
		t.Fatalf("form body: %q", got.Body)
	}
}

func TestHTTPRedirects(t *testing.T) {
	var other atomic.Int32
	var base string
	v, up := httpEnv(t, nil, func(p string) string { base = p; return httpTool("lookup", p, "") })
	enableAndKey(t, v, "lookup")
	up.h = func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/items/same"):
			http.Redirect(w, r, "https://api.example.test:"+base+"/final", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/v1/items/other"):
			http.Redirect(w, r, "https://evil.example.test:"+base+"/final", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/v1/items/port"):
			http.Redirect(w, r, "https://api.example.test:1/final", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/v1/items/plain"):
			http.Redirect(w, r, "http://api.example.test:"+base+"/final", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/v1/items/loop"):
			http.Redirect(w, r, "/v1/items/loop", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/v1/items/meta"):
			http.Redirect(w, r, "https://169.254.169.254/latest/meta-data/", http.StatusFound)
		case r.URL.Path == "/final":
			other.Add(1)
			w.Write([]byte("landed"))
		}
	}
	if out, err := v.call("work", "lookup", `{"id":"same"}`); err != nil || out.(map[string]any)["output"] != "landed" {
		t.Fatalf("same-host redirect: %v %v", out, err)
	}
	for _, id := range []string{"other", "port", "plain", "meta"} {
		before := other.Load()
		_, err := v.call("work", "lookup", fmt.Sprintf(`{"id":%q}`, id))
		if err == nil || !strings.Contains(err.Error(), "redirect") || other.Load() != before {
			t.Errorf("redirect %s: %v", id, err)
		}
	}
	if _, err := v.call("work", "lookup", `{"id":"loop"}`); err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("redirect loop: %v", err)
	}
}

func TestHTTPErrorsLimitsAndTimeouts(t *testing.T) {
	v, _ := httpEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/items/missing":
			w.WriteHeader(404)
			fmt.Fprintf(w, "not found for %s", secretToken)
		case "/v1/items/big":
			w.Write([]byte(strings.Repeat("A", 200<<10)))
		case "/v1/items/slow":
			time.Sleep(3 * time.Second)
		case "/v1/items/edge": // the token straddles the 1 KB cap
			w.Write([]byte(strings.Repeat("B", 1020) + secretToken + "tail"))
		}
	}, func(p string) string { return strings.Replace(httpTool("lookup", p, ""), "timeout 3", "timeout 1", 1) })
	enableAndKey(t, v, "lookup")

	_, err := v.call("work", "lookup", `{"id":"missing"}`)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), secretToken) {
		t.Fatalf("non-2xx: %v", err)
	}
	out, err := v.call("work", "lookup", `{"id":"big"}`)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["truncated"] != true || len(m["output"].(string)) > 1024+200 {
		t.Fatalf("size cap: truncated=%v len=%d", m["truncated"], len(m["output"].(string)))
	}
	out, err = v.call("work", "lookup", `{"id":"edge"}`)
	if err != nil {
		t.Fatal(err)
	}
	text := out.(map[string]any)["output"].(string)
	if strings.Contains(text, "tok-LIVE") || strings.Contains(text, "7f3a9c") {
		t.Fatalf("a secret straddling the cap leaked: %q", text[len(text)-80:])
	}
	start := time.Now()
	if _, err := v.call("work", "lookup", `{"id":"slow"}`); err == nil || time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("timeout: %v after %v", err, time.Since(start))
	}
}

func TestHTTPDefaultVerifiesCertificates(t *testing.T) {
	v, up := httpEnv(t, okHandler, func(p string) string { return httpTool("lookup", p, "") })
	enableAndKey(t, v, "lookup")
	x := testExec()
	x.insecureTLS = false
	v.e.Executors[toolkdl.KindHTTP] = x
	if _, err := v.call("work", "lookup", `{"id":"1"}`); err == nil || !strings.Contains(strings.ToLower(err.Error()), "certificate") {
		t.Fatalf("an untrusted certificate must fail: %v", err)
	}
	_ = up
}

func TestHTTPNoProxyFromEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	v, up := httpEnv(t, okHandler, func(p string) string { return httpTool("lookup", p, "") })
	enableAndKey(t, v, "lookup")
	if _, err := v.call("work", "lookup", `{"id":"1"}`); err != nil || up.count() != 1 {
		t.Fatalf("a proxy variable must not divert tool traffic: %v", err)
	}
}

// ---- SSRF ----

func TestBlockedAddresses(t *testing.T) {
	x := &HTTPExecutor{}
	for _, a := range []string{
		"127.0.0.1", "127.1.2.3", "::1", "10.0.0.5", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "169.254.0.1", "fe80::1", "fd00:ec2::254", "fc00::1", "100.64.0.1", "100.100.100.200",
		"0.0.0.0", "::", "224.0.0.1", "255.255.255.255", "240.0.0.1", "192.0.0.8", "198.18.0.1",
		"::ffff:10.0.0.1", "::ffff:127.0.0.1", "::ffff:169.254.169.254",
		"64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "2002:7f00:1::1", "2002:a9fe:a9fe::1", "2002:c0a8:1::1",
	} {
		if !x.blocked(netip.MustParseAddr(a)) {
			t.Errorf("%s must be blocked", a)
		}
	}
	for _, a := range []string{"93.184.216.34", "8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "172.32.0.1", "100.128.0.1", "64:ff9b::808:808", "2002:808:808::1"} {
		if x.blocked(netip.MustParseAddr(a)) {
			t.Errorf("%s must be allowed", a)
		}
	}
	lb := &HTTPExecutor{allowLoopback: true}
	if lb.blocked(netip.MustParseAddr("127.0.0.1")) || !lb.blocked(netip.MustParseAddr("10.0.0.1")) || !lb.blocked(netip.MustParseAddr("169.254.169.254")) {
		t.Error("the test hook may only open loopback")
	}
}

func dialErr(x *HTTPExecutor, addr string) error {
	c, err := x.dialContext(context.Background(), "tcp", addr)
	if c != nil {
		c.Close()
	}
	return err
}

func TestSSRFHostnamesResolvingToPrivateAddressesAreRefused(t *testing.T) {
	answers := map[string][]string{
		"private.test":   {"10.1.2.3"},
		"loop.test":      {"127.0.0.1"},
		"meta.test":      {"169.254.169.254"},
		"mapped.test":    {"::ffff:10.0.0.1"},
		"mixed.test":     {"93.184.216.34", "192.168.0.10"}, // a public answer must not excuse a private one
		"v6private.test": {"fd12::1"},
	}
	var dialed []string
	x := &HTTPExecutor{
		lookup: func(ctx context.Context, h string) ([]netip.Addr, error) {
			var out []netip.Addr
			for _, a := range answers[h] {
				out = append(out, netip.MustParseAddr(a))
			}
			return out, nil
		},
		dial: func(ctx context.Context, n, a string) (net.Conn, error) {
			dialed = append(dialed, a)
			return nil, errors.New("stub")
		},
	}
	for h := range answers {
		err := dialErr(x, h+":443")
		if err == nil || !strings.Contains(err.Error(), "refusing to connect") {
			t.Errorf("%s: %v", h, err)
		}
	}
	for _, lit := range []string{"169.254.169.254:443", "10.0.0.1:443", "[::1]:443", "[fe80::1]:443", "127.0.0.1:8443", "[::ffff:192.168.1.1]:443"} {
		if err := dialErr(x, lit); err == nil || !strings.Contains(err.Error(), "refusing to connect") {
			t.Errorf("literal %s: %v", lit, err)
		}
	}
	if len(dialed) != 0 {
		t.Fatalf("a blocked address was dialed: %v", dialed)
	}
}

// The checked address is the one dialed: a hostname whose DNS answer changes between lookups
// cannot send the connection somewhere else, because the executor looks up once and dials the IP.
func TestSSRFRebindingCannotRedirectTheConnection(t *testing.T) {
	var lookups int
	var dialed []string
	x := &HTTPExecutor{
		lookup: func(ctx context.Context, h string) ([]netip.Addr, error) {
			lookups++
			if lookups == 1 {
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil // the "second answer"
		},
		dial: func(ctx context.Context, n, a string) (net.Conn, error) {
			dialed = append(dialed, a)
			return nil, errors.New("stub")
		},
	}
	dialErr(x, "rebind.test:443")
	if lookups != 1 || len(dialed) != 1 || dialed[0] != "93.184.216.34:443" {
		t.Fatalf("lookups=%d dialed=%v: the connection must go to the checked IP, never the hostname", lookups, dialed)
	}
	// and a later dial re-resolves and is judged afresh
	if err := dialErr(x, "rebind.test:443"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("second resolution is private: %v", err)
	}
	if len(dialed) != 1 {
		t.Fatalf("dialed a private address: %v", dialed)
	}
}

func TestSSRFEndToEndRefusals(t *testing.T) {
	v := newEnv(t)
	v.e.Executors[toolkdl.KindHTTP] = &HTTPExecutor{ // production rules: no hooks except a resolver
		lookup: func(ctx context.Context, h string) ([]netip.Addr, error) {
			switch h {
			case "internal.example.test":
				return []netip.Addr{netip.MustParseAddr("10.0.0.7")}, nil
			case "metadata.example.test":
				return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
			}
			return nil, errors.New("no such host")
		},
		dial: func(ctx context.Context, n, a string) (net.Conn, error) {
			t.Errorf("dialed %s", a)
			return nil, errors.New("stub")
		}}
	for i, u := range []string{
		"https://internal.example.test/x", "https://metadata.example.test/latest/meta-data/iam",
		"https://169.254.169.254/latest/meta-data/", "https://127.0.0.1:9/x", "https://[::1]/x", "https://10.0.0.1/",
		"https://[fd00:ec2::254]/latest/meta-data/",
	} {
		name := fmt.Sprintf("ssrf%d", i)
		src := fmt.Sprintf(`tool %q { description "d"; effect "read"; run-kinds "work"; http { method "GET"; url %q } }`, name, u)
		if _, err := v.svc.Add(context.Background(), src); err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		v.svc.Enable(context.Background(), "alpha", name)
		_, err := v.call("work", name, `{}`)
		if err == nil || !strings.Contains(err.Error(), "refusing to connect") {
			t.Errorf("%s: %v", u, err)
		}
	}
	if rows := v.audit(t); len(rows) != 7 {
		t.Fatalf("every refused call is still audited: %v", rows)
	}
}

func TestHTTPHostNeverComesFromArguments(t *testing.T) {
	// Definitions cannot even be written that way (toolkdl rejects a placeholder in the host)...
	for _, u := range []string{"https://{{input.h}}/x", "https://api.example.test.{{input.h}}/x", "https://api.example.test@{{input.h}}/x", "{{input.h}}://a.example.test/"} {
		_, err := toolkdl.Parse([]byte(fmt.Sprintf(`tool "t" { description "d"; effect "read"; run-kinds "work"; input { param "h" type="string" { max-length 9 } }; http { method "GET"; url %q } }`, u)))
		if err == nil {
			t.Errorf("%s accepted", u)
		}
	}
	// ...and buildURL keeps the authority intact whatever an argument holds.
	in := &toolkdl.Input{Args: map[string]string{"p": "evil.example.test/../../x?y=1#z", "q": "@evil.example.test:443"}}
	for _, tmpl := range []string{"https://api.example.test/v1/{{input.p}}", "https://api.example.test/v1?x={{input.q}}", "https://api.example.test:8443/{{input.q}}"} {
		u, err := buildURL(tmpl, in)
		if err != nil {
			t.Fatal(err)
		}
		if h := u.Hostname(); h != "api.example.test" || u.User != nil || u.Fragment != "" {
			t.Errorf("%s -> %s (host %s)", tmpl, u, h)
		}
	}
}

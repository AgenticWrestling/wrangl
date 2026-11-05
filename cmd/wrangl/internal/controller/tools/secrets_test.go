package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/toolkdl"
)

func withStore(s *Service) map[string]string {
	m := map[string]string{}
	s.PutSecret = func(name string, r io.Reader) error {
		b, _ := io.ReadAll(r)
		m[name] = strings.TrimSpace(string(b))
		return nil
	}
	s.GetSecret = func(name string) ([]byte, error) {
		if v, ok := m[name]; ok {
			return []byte(v), nil
		}
		return nil, os.ErrNotExist
	}
	return m
}

const httpKDL = `tool "tracker" {
  description "Look up an issue"
  effect "read"
  run-kinds "work"
  input { param "id" type="integer" { min 1; max 99999 } }
  http {
    method "GET"
    url "https://api.example.com/issues/{{input.id}}"
    header "Authorization" "Bearer {{secret.tracker.token}}"
  }
}`

func TestStatusSetResolve(t *testing.T) {
	ctx := context.Background()
	s := newSvc(t)
	store := withStore(s)
	s.Add(ctx, kdl)
	s.Add(ctx, httpKDL)

	st, err := s.SecretStatuses(ctx, "alpha", "deploy_status")
	if err != nil || len(st) != 1 || st[0].Set || st[0].Env != "KUBECONFIG" || st[0].As != "file" {
		t.Fatalf("status: %v %+v", err, st)
	}
	if got := Missing(st); len(got) != 1 || got[0] != "kube.prod" {
		t.Fatalf("missing: %v", got)
	}

	if err := s.SetSecret(ctx, "alpha", "kube.prod", strings.NewReader("KUBE-CONFIG-VALUE\nline-two-of-config\n")); err != nil {
		t.Fatal(err)
	}
	if _, ok := store["tool.alpha.kube.prod"]; !ok || len(store) != 1 {
		t.Fatalf("namespacing: %v", store)
	}
	if err := s.SetSecret(ctx, "alpha", "ghost", strings.NewReader("x")); !errors.Is(err, errs.Invalid) {
		t.Fatalf("undeclared: %v", err)
	}
	if err := s.SetSecret(ctx, "nope", "kube.prod", strings.NewReader("x")); !errors.Is(err, errs.NotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	if st, _ := s.SecretStatuses(ctx, "alpha", "deploy_status"); !st[0].Set {
		t.Fatal("alpha should be set")
	}
	if st, _ := s.SecretStatuses(ctx, "beta", "deploy_status"); st[0].Set {
		t.Fatal("beta must be missing")
	}

	// exec resolution: env and file semantics
	d, _ := toolkdl.Parse([]byte(kdl))
	if _, err := s.Resolve("beta", d); err == nil {
		t.Fatal("beta has no value")
	} else {
		var me *MissingError
		if !errors.As(err, &me) || me.Names[0] != "kube.prod" || strings.Contains(err.Error(), "KUBE-CONFIG") {
			t.Fatalf("missing error: %v", err)
		}
	}
	r, err := s.Resolve("alpha", d)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	env, err := r.Materialize(dir)
	if err != nil || len(env) != 1 || !strings.HasPrefix(env[0], "KUBECONFIG="+dir+"/") {
		t.Fatalf("materialize: %v %v", err, env)
	}
	path := strings.TrimPrefix(env[0], "KUBECONFIG=")
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", err, fi)
	}
	if b, _ := os.ReadFile(path); !bytes.Contains(b, []byte("KUBE-CONFIG-VALUE")) {
		t.Fatalf("file content: %q", b)
	}
	if strings.Contains(strings.Join(env, "\n"), "KUBE-CONFIG-VALUE") {
		t.Fatal("as=file must not put the value in the environment")
	}

	// as=env
	d.Exec.Secrets[0].As = "env"
	r, _ = s.Resolve("alpha", d)
	env, _ = r.Materialize(dir)
	if !strings.HasPrefix(env[0], "KUBECONFIG=KUBE-CONFIG-VALUE") {
		t.Fatalf("env: %v", env)
	}

	// http placeholder expansion
	s.SetSecret(ctx, "alpha", "tracker.token", strings.NewReader("tok-abc123"))
	h, _ := toolkdl.Parse([]byte(httpKDL))
	got, hr, err := s.Expand("alpha", h.HTTP.Headers[0].Value)
	if err != nil || got != "Bearer tok-abc123" {
		t.Fatalf("expand: %v %q", err, got)
	}
	if out := hr.Scrubber().ScrubString("echo: " + got); strings.Contains(out, "tok-abc123") {
		t.Fatalf("scrub: %q", out)
	}
	if _, _, err := s.Expand("beta", h.HTTP.Headers[0].Value); err == nil || !strings.Contains(err.Error(), "tracker.token") {
		t.Fatalf("expand missing: %v", err)
	}
}

func TestScrubber(t *testing.T) {
	secret := "p@ss/w0rd+=xyz"
	multi := "-----BEGIN KEY-----\nAAAABBBBCCCCDDDD\nEEEEFFFFGGGGHHHH\n-----END KEY-----"
	cases := []struct {
		name, in string
		secrets  []string
		leaks    []string
	}{
		{"exact", "token is " + secret + " ok", []string{secret}, []string{secret}},
		{"twice", secret + secret, []string{secret}, []string{secret}},
		{"std b64", "x " + base64.StdEncoding.EncodeToString([]byte(secret)), []string{secret}, []string{base64.StdEncoding.EncodeToString([]byte(secret))}},
		{"raw url b64", base64.RawURLEncoding.EncodeToString([]byte(secret)), []string{secret}, []string{base64.RawURLEncoding.EncodeToString([]byte(secret))}},
		{"query escaped", "?k=" + url.QueryEscape(secret), []string{secret}, []string{url.QueryEscape(secret)}},
		{"one line of many", "line: EEEEFFFFGGGGHHHH", []string{multi}, []string{"EEEEFFFFGGGGHHHH"}},
		{"two secrets", "a=alpha-secret b=beta-secret", []string{"alpha-secret", "beta-secret"}, []string{"alpha-secret", "beta-secret"}},
		{"overlap", "abcdefgh", []string{"abcde", "cdefgh"}, []string{"abcde", "cdefgh", "abcdefgh"}},
		{"short", "the pw is xy", []string{"xy"}, []string{"xy"}},
	}
	for _, c := range cases {
		var vs [][]byte
		for _, s := range c.secrets {
			vs = append(vs, []byte(s))
		}
		out := string(NewScrubber(vs...).Scrub([]byte(c.in)))
		for _, l := range c.leaks {
			if strings.Contains(out, l) {
				t.Errorf("%s: %q still contains %q", c.name, out, l)
			}
		}
		if !strings.Contains(out, Redacted) {
			t.Errorf("%s: nothing redacted: %q", c.name, out)
		}
	}
	// the placeholder cannot be re-matched and unrelated text survives
	if got := string(NewScrubber([]byte("ED")).Scrub([]byte("SECRED and ED"))); strings.Contains(got, "ED]") && got != "SECR[REDACTED] and [REDACTED]" {
		t.Errorf("replacement was re-scrubbed: %q", got)
	}
	if got := string(NewScrubber([]byte("zzz")).Scrub([]byte("clean output"))); got != "clean output" {
		t.Errorf("clean output changed: %q", got)
	}
	var nilS *Scrubber
	if string(nilS.Scrub([]byte("x"))) != "x" || string(NewScrubber().Scrub([]byte("x"))) != "x" {
		t.Error("empty scrubber must be a no-op")
	}
}

package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/toolkdl"
)

// SecretStatus says whether one declared secret has a value for a project. Never the value.
type SecretStatus struct {
	toolkdl.SecretSpec
	Set bool `json:"set"`
}

// StoreName is the secret store file backing a tool secret of a project: tool.<slug>.<name>.
// Project slugs contain no dots, so the pair is unambiguous, and a tool can only ever reach
// names under its own project's prefix, never agent or integration secrets.
func StoreName(slug, name string) string { return "tool." + slug + "." + name }

var placeholderRe = regexp.MustCompile(`\{\{\s*secret\.([A-Za-z0-9_.-]*)\s*\}\}`)

func (s *Service) definition(ctx context.Context, name string) (Tool, *toolkdl.Definition, error) {
	t, err := s.Get(ctx, name)
	if err != nil {
		return t, nil, err
	}
	d, err := toolkdl.Parse([]byte(t.Kdl))
	if err != nil { // stored definitions were validated on the way in
		return t, nil, fmt.Errorf("stored definition of %q no longer parses: %v", name, err)
	}
	return t, d, nil
}

// SecretStatuses reports set/missing for every secret the tool declares, for one project.
func (s *Service) SecretStatuses(ctx context.Context, slug, name string) ([]SecretStatus, error) {
	if _, err := s.project(ctx, slug); err != nil {
		return nil, err
	}
	_, d, err := s.definition(ctx, name)
	if err != nil {
		return nil, err
	}
	return s.statuses(slug, d), nil
}

func (s *Service) statuses(slug string, d *toolkdl.Definition) []SecretStatus {
	out := []SecretStatus{}
	for _, sp := range d.SecretSpecs() {
		out = append(out, SecretStatus{SecretSpec: sp, Set: s.has(StoreName(slug, sp.Name))})
	}
	return out
}

func (s *Service) has(store string) bool {
	if s.GetSecret == nil {
		return false
	}
	v, err := s.GetSecret(store)
	return err == nil && len(v) > 0
}

// SetSecret stores the value read from r for a secret that some library tool declares, in a
// project's own namespace. The value is never echoed, logged or put in an error.
func (s *Service) SetSecret(ctx context.Context, slug, name string, r io.Reader) error {
	if s.PutSecret == nil {
		return fmt.Errorf("%w: secrets cannot be managed on this controller", errs.Invalid)
	}
	if _, err := s.project(ctx, slug); err != nil {
		return err
	}
	all, err := s.List(ctx)
	if err != nil {
		return err
	}
	declared := false
	for _, t := range all {
		d, err := toolkdl.Parse([]byte(t.Kdl))
		if err != nil {
			continue
		}
		for _, sp := range d.SecretSpecs() {
			declared = declared || sp.Name == name
		}
	}
	if !declared {
		return fmt.Errorf("%w: no library tool declares a secret named %q", errs.Invalid, name)
	}
	if err := s.PutSecret(StoreName(slug, name), r); err != nil {
		return fmt.Errorf("%w: storing secret %q: %s", errs.Invalid, name, errText(err))
	}
	if s.Log != nil {
		s.Log.Info("tool secret set", "project", slug, "secret", name)
	}
	return nil
}

// errText keeps store errors to their message: the store reports names, never values.
func errText(err error) string { return err.Error() }

// Missing lists the declared secrets of a tool that have no value in the project.
func Missing(st []SecretStatus) []string {
	var m []string
	for _, x := range st {
		if !x.Set {
			m = append(m, x.Name)
		}
	}
	return m
}

// Resolved holds the secret values for one call. It is for the executors; nothing in it may be
// logged, returned by the API or shipped to a runner.
type Resolved struct {
	envs   []resolvedEnv
	values [][]byte
}

type resolvedEnv struct {
	Env, As string
	Value   []byte
}

// MissingError names the secrets without a value.
type MissingError struct{ Names []string }

func (e *MissingError) Error() string {
	return "missing secret(s): " + strings.Join(e.Names, ", ")
}

// Resolve loads the secrets of an exec tool for a project (as=env|file per declaration).
func (s *Service) Resolve(slug string, d *toolkdl.Definition) (*Resolved, error) {
	r := &Resolved{}
	var missing []string
	for _, sp := range d.SecretSpecs() {
		v, err := s.fetch(slug, sp.Name)
		if err != nil {
			missing = append(missing, sp.Name)
			continue
		}
		r.values = append(r.values, v)
		if sp.Env != "" {
			r.envs = append(r.envs, resolvedEnv{Env: sp.Env, As: sp.As, Value: v})
		}
	}
	if len(missing) > 0 {
		return nil, &MissingError{Names: missing}
	}
	return r, nil
}

func (s *Service) fetch(slug, name string) ([]byte, error) {
	if s.GetSecret == nil {
		return nil, errors.New("no secret store")
	}
	v, err := s.GetSecret(StoreName(slug, name))
	if err != nil || len(v) == 0 {
		return nil, errors.New("missing")
	}
	return v, nil
}

// Expand replaces {{secret.NAME}} in s (a header value or body of an http/mcp-remote tool) with
// the project's values and returns the values used, for scrubbing.
func (s *Service) Expand(slug, in string) (string, *Resolved, error) {
	r := &Resolved{}
	var missing []string
	out := placeholderRe.ReplaceAllStringFunc(in, func(m string) string {
		name := placeholderRe.FindStringSubmatch(m)[1]
		v, err := s.fetch(slug, name)
		if err != nil {
			missing = append(missing, name)
			return ""
		}
		r.values = append(r.values, v)
		return string(v)
	})
	if len(missing) > 0 {
		return "", nil, &MissingError{Names: missing}
	}
	return out, r, nil
}

// Merge adds the values of o to r.
func (r *Resolved) Merge(o *Resolved) { r.values = append(r.values, o.values...) }

// Materialize returns the process environment for the secrets: NAME=value for as=env, and for
// as=file a 0600 file under dir (which must be private to the call) with NAME=<path>.
func (r *Resolved) Materialize(dir string) ([]string, error) {
	var env []string
	for _, e := range r.envs {
		if e.As != "file" {
			env = append(env, e.Env+"="+string(e.Value))
			continue
		}
		var rnd [8]byte
		rand.Read(rnd[:])
		p := filepath.Join(dir, "secret-"+strings.ToLower(e.Env)+"-"+hex.EncodeToString(rnd[:]))
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, fmt.Errorf("secret file for %s: %w", e.Env, err)
		}
		if _, err := f.Write(e.Value); err != nil {
			f.Close()
			return nil, fmt.Errorf("secret file for %s: write failed", e.Env)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("secret file for %s: close failed", e.Env)
		}
		env = append(env, e.Env+"="+p)
	}
	return env, nil
}

// Scrubber returns the exact-value scrubber for the call's secrets.
func (r *Resolved) Scrubber() *Scrubber { return NewScrubber(r.values...) }

// Redacted replaces scrubbed values.
const Redacted = "[REDACTED]"

// Scrubber removes exact secret values from output. It also removes the base64 (standard, URL,
// padded or not) and URL-encoded forms, and each line of a multi-line value (a kubeconfig
// printed line by line), so a tool echoing a credential in a common encoding does not leak it.
// Nothing is skipped for being short: a one-character secret will shred output, which is
// better than leaking it.
type Scrubber struct{ needles [][]byte }

// NewScrubber builds a scrubber for values.
func NewScrubber(values ...[]byte) *Scrubber {
	seen := map[string]bool{}
	var ns [][]byte
	add := func(b []byte) {
		if len(b) == 0 || seen[string(b)] {
			return
		}
		seen[string(b)] = true
		ns = append(ns, append([]byte(nil), b...))
	}
	for _, v := range values {
		add(v)
		add([]byte(base64.StdEncoding.EncodeToString(v)))
		add([]byte(base64.RawStdEncoding.EncodeToString(v)))
		add([]byte(base64.URLEncoding.EncodeToString(v)))
		add([]byte(base64.RawURLEncoding.EncodeToString(v)))
		add([]byte(url.QueryEscape(string(v))))
		add([]byte(url.PathEscape(string(v))))
		if bytes.ContainsAny(v, "\r\n") {
			for _, line := range bytes.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == '\r' }) {
				if t := bytes.TrimSpace(line); len(t) >= 8 {
					add(t)
				}
			}
		}
	}
	sort.SliceStable(ns, func(i, j int) bool { return len(ns[i]) > len(ns[j]) })
	return &Scrubber{needles: ns}
}

// Scrub returns b with every secret form replaced by [REDACTED]. Matches are found on the
// original bytes and merged, so a replacement can never itself be matched or split a secret.
func (s *Scrubber) Scrub(b []byte) []byte {
	if s == nil || len(s.needles) == 0 {
		return b
	}
	type span struct{ from, to int }
	var spans []span
	for _, n := range s.needles {
		for off := 0; ; {
			i := bytes.Index(b[off:], n)
			if i < 0 {
				break
			}
			spans = append(spans, span{off + i, off + i + len(n)})
			off += i + 1
		}
	}
	if len(spans) == 0 {
		return b
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].from < spans[j].from })
	var out []byte
	pos := 0
	for k := 0; k < len(spans); {
		from, to := spans[k].from, spans[k].to
		for k++; k < len(spans) && spans[k].from <= to; k++ {
			if spans[k].to > to {
				to = spans[k].to
			}
		}
		out = append(out, b[pos:from]...)
		out = append(out, Redacted...)
		pos = to
	}
	return append(out, b[pos:]...)
}

// ScrubString is Scrub for strings (error text).
func (s *Scrubber) ScrubString(in string) string { return string(s.Scrub([]byte(in))) }

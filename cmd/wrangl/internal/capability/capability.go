// Package capability is the tag and requirement vocabulary of ADR 0007 stage 2 (ADR 0009 stage 6): a
// runner has tags (facts about what it can do: arch=arm64, tool:go=1.22, agent:claude=authed), a project,
// task or run has requirements against them, and a requirement set matches a runner when every
// requirement holds. It is pure: no database.
package capability

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

// Operators of a requirement.
const (
	OpEq     = "="      // the tag has exactly this value
	OpGE     = ">="     // the tag's value is a version at least this
	OpIn     = "in"     // the tag's value is one of a|b|c
	OpExists = "exists" // the tag is present, with any value
)

// Tags are a runner's capabilities: key -> value ("" for a bare tag).
type Tags map[string]string

// Requirement is one condition on a runner's tags.
type Requirement struct {
	Key, Op, Value string
}

var keyRe = regexp.MustCompile(`^[a-z][a-z0-9_.:+-]{0,63}$`)

// ParseTag reads "key=value" or a bare "key".
func ParseTag(s string) (key, value string, err error) {
	s = strings.TrimSpace(s)
	key, value, _ = strings.Cut(s, "=")
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	if !keyRe.MatchString(key) {
		return "", "", fmt.Errorf("%w: tag %q: key must be lowercase letters, digits and . _ : + -", errs.Invalid, s)
	}
	if strings.ContainsAny(value, " \t,") {
		return "", "", fmt.Errorf("%w: tag %q: value has no spaces or commas", errs.Invalid, s)
	}
	return key, value, nil
}

// Parse reads a requirement: "arch=arm64", "tool:go>=1.22", "net=public|vpn" (one of), or a bare
// "agent:claude" (the tag exists).
func Parse(s string) (Requirement, error) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ">="); i > 0 {
		return build(s[:i], OpGE, s[i+2:], s)
	}
	if i := strings.Index(s, "="); i > 0 {
		v := strings.TrimSpace(s[i+1:])
		if strings.Contains(v, "|") {
			return build(s[:i], OpIn, v, s)
		}
		return build(s[:i], OpEq, v, s)
	}
	return build(s, OpExists, "", s)
}

func build(key, op, value, orig string) (Requirement, error) {
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	if !keyRe.MatchString(key) {
		return Requirement{}, fmt.Errorf("%w: requirement %q: key must be lowercase letters, digits and . _ : + -", errs.Invalid, orig)
	}
	if op != OpExists && value == "" {
		return Requirement{}, fmt.Errorf("%w: requirement %q needs a value", errs.Invalid, orig)
	}
	if op == OpGE && versionParts(value) == nil {
		return Requirement{}, fmt.Errorf("%w: requirement %q: >= wants a dotted number like 1.22", errs.Invalid, orig)
	}
	if strings.ContainsAny(value, " \t,") {
		return Requirement{}, fmt.Errorf("%w: requirement %q: value has no spaces or commas", errs.Invalid, orig)
	}
	return Requirement{Key: key, Op: op, Value: value}, nil
}

// String is the canonical text form (what Parse reads back).
func (r Requirement) String() string {
	switch r.Op {
	case OpGE:
		return r.Key + ">=" + r.Value
	case OpEq, OpIn:
		return r.Key + "=" + r.Value
	}
	return r.Key
}

func versionParts(v string) []int {
	var out []int
	for _, p := range strings.Split(strings.TrimPrefix(v, "v"), ".") {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// atLeast compares dotted versions numerically (1.22 >= 1.9; missing parts are 0).
func atLeast(have, want string) bool {
	h, w := versionParts(have), versionParts(want)
	if h == nil || w == nil {
		return false
	}
	for i := 0; i < max(len(h), len(w)); i++ {
		var a, b int
		if i < len(h) {
			a = h[i]
		}
		if i < len(w) {
			b = w[i]
		}
		if a != b {
			return a > b
		}
	}
	return true
}

// Holds reports whether the tags satisfy one requirement.
func (r Requirement) Holds(t Tags) bool {
	v, ok := t[r.Key]
	if !ok {
		return false
	}
	switch r.Op {
	case OpExists:
		return true
	case OpEq:
		return v == r.Value
	case OpGE:
		return atLeast(v, r.Value)
	case OpIn:
		for _, c := range strings.Split(r.Value, "|") {
			if v == c {
				return true
			}
		}
	}
	return false
}

// Match reports whether the tags satisfy every requirement. An empty set matches anything. A runner with
// no tags at all (not yet described, e.g. enrolling) is unrestricted: it is not held back by requirements
// it could not have been asked about.
func Match(reqs []Requirement, t Tags) bool {
	if len(reqs) == 0 || len(t) == 0 {
		return true
	}
	for _, r := range reqs {
		if !r.Holds(t) {
			return false
		}
	}
	return true
}

// Format lists tags as sorted "key=value" strings.
func (t Tags) Format() []string {
	out := make([]string, 0, len(t))
	for k, v := range t {
		if v == "" {
			out = append(out, k)
		} else {
			out = append(out, k+"="+v)
		}
	}
	sort.Strings(out)
	return out
}

// ParseTags reads "key=value" strings.
func ParseTags(ss []string) (Tags, error) {
	t := Tags{}
	for _, s := range ss {
		k, v, err := ParseTag(s)
		if err != nil {
			return nil, err
		}
		t[k] = v
	}
	return t, nil
}

// IsVersion reports whether s is a dotted number such as 1.22 or v18.
func IsVersion(s string) bool { return s != "" && versionParts(s) != nil }

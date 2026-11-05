package toolkdl

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/kdl"
)

var (
	nameRe      = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)
	paramRe     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	envRe       = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	secretRe    = regexp.MustCompile(`^[a-z0-9_.-]{1,100}$`)
	headerRe    = regexp.MustCompile("^[A-Za-z0-9-]{1,64}$")
	placeholder = regexp.MustCompile(`\{\{\s*([a-z]+)\.([A-Za-z0-9_.-]*)\s*\}\}`)
	wholeInput  = regexp.MustCompile(`^\{\{input\.([a-z][a-z0-9_]{0,39})\}\}$`)
	toolRe      = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

var runKinds = map[string]bool{"work": true, "revise": true, "resolve": true, "review": true, "plan": true}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "csh": true, "tcsh": true, "ash": true, "busybox": true, "env": true}

var sensitiveHeader = regexp.MustCompile(`(?i)auth|token|secret|key|password|cookie|credential`)

// Parse parses and validates a file holding exactly one tool node.
func Parse(src []byte) (*Definition, error) {
	nodes, err := kdl.Parse(src)
	if err != nil {
		return nil, err
	}
	if len(nodes) != 1 || nodes[0].Name != "tool" {
		return nil, fmt.Errorf("tool file must contain exactly one top-level tool node")
	}
	return build(nodes[0])
}

// node helpers --------------------------------------------------------------

func strArg(n *kdl.Node) (string, error) {
	if len(n.Args) != 1 {
		return "", fmt.Errorf("%s: want exactly one argument", n.Name)
	}
	s, ok := n.Args[0].(string)
	if !ok {
		return "", fmt.Errorf("%s: argument must be a string", n.Name)
	}
	return s, nil
}

func intArg(n *kdl.Node) (int64, error) {
	if len(n.Args) != 1 {
		return 0, fmt.Errorf("%s: want exactly one argument", n.Name)
	}
	i, ok := n.Args[0].(int64)
	if !ok {
		return 0, fmt.Errorf("%s: argument must be an integer", n.Name)
	}
	return i, nil
}

func strArgs(n *kdl.Node) ([]string, error) {
	var out []string
	for _, a := range n.Args {
		s, ok := a.(string)
		if !ok {
			return nil, fmt.Errorf("%s: arguments must be strings", n.Name)
		}
		out = append(out, s)
	}
	return out, nil
}

func noProps(n *kdl.Node, allowed ...string) error {
	for k := range n.Props {
		ok := false
		for _, a := range allowed {
			ok = ok || a == k
		}
		if !ok {
			return fmt.Errorf("%s: unknown property %q", n.Name, k)
		}
	}
	return nil
}

func noChildren(n *kdl.Node) error {
	if len(n.Children) > 0 {
		return fmt.Errorf("%s: unexpected child nodes", n.Name)
	}
	return nil
}

func propStr(n *kdl.Node, k string) (string, bool, error) {
	v, ok := n.Props[k]
	if !ok {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", true, fmt.Errorf("%s: property %q must be a string", n.Name, k)
	}
	return s, true, nil
}

// single indexes children by name, rejecting duplicates of names not in repeatable.
func single(n *kdl.Node, allowed map[string]bool) (map[string]*kdl.Node, error) {
	m := map[string]*kdl.Node{}
	for _, c := range n.Children {
		if _, ok := allowed[c.Name]; !ok {
			return nil, fmt.Errorf("%s: unknown node %q", n.Name, c.Name)
		}
		if !allowed[c.Name] {
			if _, dup := m[c.Name]; dup {
				return nil, fmt.Errorf("%s: duplicate %q", n.Name, c.Name)
			}
		}
		m[c.Name] = c
	}
	return m, nil
}

// build ---------------------------------------------------------------------

func build(t *kdl.Node) (*Definition, error) {
	name, err := strArg(t)
	if err != nil {
		return nil, err
	}
	if err := noProps(t); err != nil {
		return nil, err
	}
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("tool name %q must match %s", name, nameRe)
	}
	d := &Definition{Name: name}
	// true = may repeat
	idx, err := single(t, map[string]bool{"description": false, "effect": false, "run-kinds": false, "input": false, "exec": false, "http": false, "mcp-remote": false})
	if err != nil {
		return nil, err
	}
	for _, k := range []string{"description", "effect", "run-kinds"} {
		if idx[k] == nil {
			return nil, fmt.Errorf("tool %s: %s is required", name, k)
		}
	}
	for _, k := range []string{"description", "effect"} {
		if err := noProps(idx[k]); err != nil {
			return nil, err
		}
		if err := noChildren(idx[k]); err != nil {
			return nil, err
		}
	}
	if d.Description, err = strArg(idx["description"]); err != nil {
		return nil, err
	}
	if strings.TrimSpace(d.Description) == "" || len(d.Description) > MaxDescription {
		return nil, fmt.Errorf("description must be 1..%d bytes", MaxDescription)
	}
	e, err := strArg(idx["effect"])
	if err != nil {
		return nil, err
	}
	switch Effect(e) {
	case EffectRead, EffectWrite, EffectDestructive:
		d.Effect = Effect(e)
	default:
		return nil, fmt.Errorf("effect %q must be read, write or destructive", e)
	}
	rk := idx["run-kinds"]
	if err := noProps(rk); err != nil {
		return nil, err
	}
	if err := noChildren(rk); err != nil {
		return nil, err
	}
	if d.RunKinds, err = strArgs(rk); err != nil {
		return nil, err
	}
	if len(d.RunKinds) == 0 {
		return nil, fmt.Errorf("run-kinds needs at least one kind")
	}
	seen := map[string]bool{}
	for _, k := range d.RunKinds {
		if !runKinds[k] {
			return nil, fmt.Errorf("unknown run kind %q", k)
		}
		if seen[k] {
			return nil, fmt.Errorf("duplicate run kind %q", k)
		}
		seen[k] = true
		if (k == "review" || k == "plan") && d.Effect != EffectRead {
			return nil, fmt.Errorf("run kind %q is read-only: only effect \"read\" tools may list it", k)
		}
	}

	execs := 0
	for _, k := range []string{"exec", "http", "mcp-remote"} {
		if idx[k] != nil {
			execs++
		}
	}
	if execs != 1 {
		return nil, fmt.Errorf("tool %s needs exactly one of exec, http, mcp-remote", name)
	}

	if in := idx["input"]; in != nil {
		if d.Params, err = buildInput(in); err != nil {
			return nil, err
		}
	}
	params := map[string]*Param{}
	for i := range d.Params {
		params[d.Params[i].Name] = &d.Params[i]
	}

	switch {
	case idx["exec"] != nil:
		d.Kind = KindExec
		d.Exec, err = buildExec(idx["exec"], params)
	case idx["http"] != nil:
		d.Kind = KindHTTP
		d.HTTP, err = buildHTTP(idx["http"], params)
	default:
		d.Kind = KindMCPRemote
		if idx["input"] != nil {
			return nil, fmt.Errorf("mcp-remote tools take no input block (the upstream server declares its own)")
		}
		d.MCPRemote, err = buildMCPRemote(idx["mcp-remote"])
	}
	if err != nil {
		return nil, fmt.Errorf("tool %s: %w", name, err)
	}
	return d, nil
}

func buildInput(in *kdl.Node) ([]Param, error) {
	if len(in.Args) > 0 || len(in.Props) > 0 {
		return nil, fmt.Errorf("input: takes only param children")
	}
	var ps []Param
	seen := map[string]bool{}
	for _, c := range in.Children {
		if c.Name != "param" {
			return nil, fmt.Errorf("input: unknown node %q", c.Name)
		}
		p, err := buildParam(c)
		if err != nil {
			return nil, err
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("duplicate param %q", p.Name)
		}
		seen[p.Name] = true
		ps = append(ps, *p)
	}
	if len(ps) > MaxParams {
		return nil, fmt.Errorf("at most %d params", MaxParams)
	}
	return ps, nil
}

func buildParam(n *kdl.Node) (*Param, error) {
	name, err := strArg(n)
	if err != nil {
		return nil, err
	}
	if !paramRe.MatchString(name) {
		return nil, fmt.Errorf("param name %q must match %s", name, paramRe)
	}
	if err := noProps(n, "type", "description", "required", "allow-dash"); err != nil {
		return nil, fmt.Errorf("param %s: %w", name, err)
	}
	p := &Param{Name: name, Required: true, MaxLen: DefaultMaxLen}
	if p.Type, _, err = propStr(n, "type"); err != nil {
		return nil, err
	}
	switch p.Type {
	case "string", "integer", "number", "boolean":
	default:
		return nil, fmt.Errorf("param %s: type %q must be string, integer, number or boolean", name, p.Type)
	}
	if p.Description, _, err = propStr(n, "description"); err != nil {
		return nil, err
	}
	for k, dst := range map[string]*bool{"required": &p.Required, "allow-dash": &p.AllowDash} {
		if v, ok := n.Props[k]; ok {
			b, ok := v.(bool)
			if !ok {
				return nil, fmt.Errorf("param %s: %s must be #true or #false", name, k)
			}
			*dst = b
		}
	}
	idx, err := single(n, map[string]bool{"enum": false, "pattern": false, "min": false, "max": false, "min-length": false, "max-length": false})
	if err != nil {
		return nil, fmt.Errorf("param %s: %w", name, err)
	}
	for k, c := range idx {
		if err := noProps(c); err != nil {
			return nil, err
		}
		if err := noChildren(c); err != nil {
			return nil, err
		}
		switch k {
		case "enum":
			if p.Type != "string" && p.Type != "integer" {
				return nil, fmt.Errorf("param %s: enum needs type string or integer", name)
			}
			for _, a := range c.Args {
				switch v := a.(type) {
				case string:
					if p.Type != "string" {
						return nil, fmt.Errorf("param %s: enum values must be integers", name)
					}
					p.Enum = append(p.Enum, v)
				case int64:
					if p.Type != "integer" {
						return nil, fmt.Errorf("param %s: enum values must be strings", name)
					}
					p.Enum = append(p.Enum, fmt.Sprint(v))
				default:
					return nil, fmt.Errorf("param %s: bad enum value", name)
				}
			}
			if len(p.Enum) == 0 || len(p.Enum) > 256 {
				return nil, fmt.Errorf("param %s: enum needs 1..256 values", name)
			}
		case "pattern":
			if p.Type != "string" {
				return nil, fmt.Errorf("param %s: pattern needs type string", name)
			}
			if p.Pattern, err = strArg(c); err != nil {
				return nil, err
			}
		case "min", "max":
			if p.Type != "integer" {
				return nil, fmt.Errorf("param %s: %s needs type integer", name, k)
			}
			v, err := intArg(c)
			if err != nil {
				return nil, err
			}
			if k == "min" {
				p.Min = &v
			} else {
				p.Max = &v
			}
		case "min-length", "max-length":
			if p.Type != "string" {
				return nil, fmt.Errorf("param %s: %s needs type string", name, k)
			}
			v, err := intArg(c)
			if err != nil {
				return nil, err
			}
			if v < 0 || v > HardMaxLen {
				return nil, fmt.Errorf("param %s: %s must be 0..%d", name, k, HardMaxLen)
			}
			if k == "min-length" {
				p.MinLen = int(v)
			} else {
				p.MaxLen = int(v)
			}
		}
	}
	if p.Min != nil && p.Max != nil && *p.Min > *p.Max {
		return nil, fmt.Errorf("param %s: min > max", name)
	}
	if p.MinLen > p.MaxLen {
		return nil, fmt.Errorf("param %s: min-length > max-length", name)
	}
	if p.Pattern != "" {
		if len(p.Pattern) > MaxPattern {
			return nil, fmt.Errorf("param %s: pattern longer than %d", name, MaxPattern)
		}
		if !strings.HasPrefix(p.Pattern, "^") || !strings.HasSuffix(p.Pattern, "$") || strings.HasSuffix(p.Pattern, `\$`) {
			return nil, fmt.Errorf("param %s: pattern must be anchored with ^ and $", name)
		}
		if _, err := regexp.Compile(p.Pattern); err != nil {
			return nil, fmt.Errorf("param %s: bad pattern: %w", name, err)
		}
	}
	if p.Type == "string" && len(p.Enum) == 0 && p.Pattern == "" && idx["max-length"] == nil {
		// bounded by the default; recorded so callers see the limit
		p.MaxLen = DefaultMaxLen
	}
	return p, nil
}

// permitsDash reports whether the param's declared constraints accept a value starting with '-'
// (the runtime also rejects such values unless AllowDash). It is conservative for patterns: a
// probe set of leading-dash strings is tested.
func (p *Param) permitsDash() bool {
	switch p.Type {
	case "integer":
		if len(p.Enum) > 0 {
			for _, e := range p.Enum {
				if strings.HasPrefix(e, "-") {
					return true
				}
			}
			return false
		}
		return p.Min == nil || *p.Min < 0
	case "number":
		return true
	case "string":
		for _, e := range p.Enum {
			if strings.HasPrefix(e, "-") {
				return true
			}
		}
		if len(p.Enum) > 0 {
			return false
		}
		if p.Pattern == "" {
			return true
		}
		re := regexp.MustCompile(p.Pattern)
		for _, probe := range []string{"-", "--", "-a", "-A", "-0", "-a-", "--a", "-x y", "-_", "-.", "-a1", "-1a"} {
			if re.MatchString(probe) {
				return true
			}
		}
	}
	return false
}

func secretRefs(s string) []string {
	var out []string
	for _, m := range placeholder.FindAllStringSubmatch(s, -1) {
		if m[1] == "secret" {
			out = append(out, m[2])
		}
	}
	return out
}

// checkPlaceholders validates every {{...}} in s: kinds must be input (declared param) or, when
// secrets is true, secret (a valid store name). Anything else, or an unterminated {{, is an error.
func checkPlaceholders(s string, params map[string]*Param, inputOK, secretsOK bool) error {
	rest := placeholder.ReplaceAllStringFunc(s, func(m string) string {
		return "\x00"
	})
	// A stray "}}" is fine: it closes nested JSON objects. An opening "{{" that is not a
	// well-formed placeholder is a typo that would be sent verbatim.
	if strings.Contains(rest, "{{") {
		return fmt.Errorf("malformed placeholder in %q", s)
	}
	for _, m := range placeholder.FindAllStringSubmatch(s, -1) {
		switch m[1] {
		case "input":
			if !inputOK {
				return fmt.Errorf("{{input.%s}} is not allowed here", m[2])
			}
			if params[m[2]] == nil {
				return fmt.Errorf("{{input.%s}} refers to an undeclared param", m[2])
			}
		case "secret":
			if !secretsOK {
				return fmt.Errorf("{{secret.%s}} is not allowed here (declare secrets with a secret node)", m[2])
			}
			if !secretRe.MatchString(m[2]) {
				return fmt.Errorf("bad secret name %q", m[2])
			}
		default:
			return fmt.Errorf("unknown placeholder {{%s.%s}}", m[1], m[2])
		}
	}
	return nil
}

func timeoutOf(n *kdl.Node, def, max int) (int, error) {
	if n == nil {
		return def, nil
	}
	if err := noProps(n); err != nil {
		return 0, err
	}
	if err := noChildren(n); err != nil {
		return 0, err
	}
	v, err := intArg(n)
	if err != nil {
		return 0, err
	}
	if v < 1 || v > int64(max) {
		return 0, fmt.Errorf("timeout must be 1..%d seconds", max)
	}
	return int(v), nil
}

func outputOf(n *kdl.Node) (int, error) {
	if n == nil {
		return DefaultOutputKB, nil
	}
	if err := noProps(n); err != nil {
		return 0, err
	}
	if err := noChildren(n); err != nil {
		return 0, err
	}
	v, err := intArg(n)
	if err != nil {
		return 0, err
	}
	if v < 1 || v > MaxOutputKB {
		return 0, fmt.Errorf("max-output-kb must be 1..%d", MaxOutputKB)
	}
	return int(v), nil
}

func buildExec(n *kdl.Node, params map[string]*Param) (*Exec, error) {
	if len(n.Args) > 0 || len(n.Props) > 0 {
		return nil, fmt.Errorf("exec: takes only child nodes")
	}
	idx, err := single(n, map[string]bool{"argv": false, "secret": true, "network": false, "timeout": false, "max-output-kb": false})
	if err != nil {
		return nil, err
	}
	x := &Exec{Network: "none"}
	av := idx["argv"]
	if av == nil {
		return nil, fmt.Errorf("exec: argv is required")
	}
	if err := noProps(av); err != nil {
		return nil, err
	}
	if err := noChildren(av); err != nil {
		return nil, err
	}
	if x.Argv, err = strArgs(av); err != nil {
		return nil, err
	}
	if len(x.Argv) == 0 || len(x.Argv) > MaxArgv {
		return nil, fmt.Errorf("argv needs 1..%d elements", MaxArgv)
	}
	bin := x.Argv[0]
	if bin == "" || strings.ContainsAny(bin, "\x00\n\r ") || strings.Contains(bin, "{{") || strings.HasPrefix(bin, "-") {
		return nil, fmt.Errorf("argv[0] %q must be a plain binary name or path without placeholders", bin)
	}
	if base := bin[strings.LastIndex(bin, "/")+1:]; shells[base] {
		return nil, fmt.Errorf("argv[0] %q is a shell or launcher: commands are argv-only, never a shell", bin)
	}
	used := map[string]bool{}
	for i, a := range x.Argv {
		if len(a) > MaxArgLen || strings.ContainsRune(a, 0) {
			return nil, fmt.Errorf("argv[%d] is too long or contains NUL", i)
		}
		if !strings.Contains(a, "{{") && !strings.Contains(a, "}}") {
			continue
		}
		m := wholeInput.FindStringSubmatch(a)
		if m == nil {
			return nil, fmt.Errorf("argv[%d] %q: {{input.x}} must fill a whole argv element and nothing else", i, a)
		}
		p := params[m[1]]
		if p == nil {
			return nil, fmt.Errorf("argv[%d]: {{input.%s}} refers to an undeclared param", i, m[1])
		}
		used[m[1]] = true
	}
	for name := range used {
		p := params[name]
		if p.permitsDash() && !p.AllowDash {
			return nil, fmt.Errorf("param %s is used in argv but its constraints permit a leading '-': tighten its enum/pattern/min, or declare allow-dash=#true explicitly", name)
		}
		if p.AllowDash && !p.permitsDash() {
			// harmless but misleading; keep the declaration honest
			return nil, fmt.Errorf("param %s declares allow-dash but its constraints already forbid a leading '-'", name)
		}
	}
	seenEnv := map[string]bool{}
	for _, c := range n.Children {
		if c.Name != "secret" {
			continue
		}
		s, err := buildExecSecret(c)
		if err != nil {
			return nil, err
		}
		if seenEnv[s.Env] {
			return nil, fmt.Errorf("duplicate secret env %q", s.Env)
		}
		seenEnv[s.Env] = true
		x.Secrets = append(x.Secrets, *s)
	}
	if nw := idx["network"]; nw != nil {
		if err := noProps(nw); err != nil {
			return nil, err
		}
		if err := noChildren(nw); err != nil {
			return nil, err
		}
		if x.Network, err = strArg(nw); err != nil {
			return nil, err
		}
		if x.Network != "none" && x.Network != "full" {
			return nil, fmt.Errorf("network %q must be none or full", x.Network)
		}
	}
	if x.TimeoutS, err = timeoutOf(idx["timeout"], DefaultExecTimeout, MaxExecTimeout); err != nil {
		return nil, err
	}
	if x.MaxOutputKB, err = outputOf(idx["max-output-kb"]); err != nil {
		return nil, err
	}
	return x, nil
}

func buildExecSecret(n *kdl.Node) (*ExecSecret, error) {
	if len(n.Args) != 1 {
		return nil, fmt.Errorf("secret: want exactly one argument (the env var); values are never written in a definition")
	}
	if err := noChildren(n); err != nil {
		return nil, err
	}
	if err := noProps(n, "name", "as"); err != nil {
		return nil, err
	}
	env, ok := n.Args[0].(string)
	if !ok || !envRe.MatchString(env) {
		return nil, fmt.Errorf("secret env var must match %s", envRe)
	}
	name, ok, err := propStr(n, "name")
	if err != nil {
		return nil, err
	}
	if !ok || !secretRe.MatchString(name) {
		return nil, fmt.Errorf("secret %s: name=\"...\" (the stored secret's name, matching %s) is required", env, secretRe)
	}
	as, ok, err := propStr(n, "as")
	if err != nil {
		return nil, err
	}
	if !ok {
		as = "env"
	}
	if as != "env" && as != "file" {
		return nil, fmt.Errorf("secret %s: as must be env or file", env)
	}
	return &ExecSecret{Env: env, Name: name, As: as}, nil
}

var methods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// checkURL requires https, a literal host and no credentials. inputPathOK allows {{input.x}}
// after the host (path and query only).
func checkURL(raw string, params map[string]*Param, inputPathOK bool) error {
	const pre = "https://"
	if !strings.HasPrefix(raw, pre) {
		return fmt.Errorf("url %q must start with https://", raw)
	}
	rest := raw[len(pre):]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	authority := rest[:end]
	if authority == "" || strings.ContainsAny(authority, "{}@\\ \t\r\n") || strings.ContainsRune(authority, 0) {
		return fmt.Errorf("url %q: the host must be literal, without placeholders or credentials", raw)
	}
	tail := rest[end:]
	if strings.ContainsAny(tail, " \t\r\n\\") || strings.ContainsRune(tail, 0) {
		return fmt.Errorf("url %q contains whitespace or backslash", raw)
	}
	if err := checkPlaceholders(tail, params, inputPathOK, false); err != nil {
		return fmt.Errorf("url: %w", err)
	}
	clean := placeholder.ReplaceAllString(raw, "x")
	u, err := url.Parse(clean)
	if err != nil || u.Host == "" || u.User != nil || u.Scheme != "https" {
		return fmt.Errorf("url %q is not valid", raw)
	}
	if u.Hostname() == "" || u.Fragment != "" {
		return fmt.Errorf("url %q: bad host or fragment", raw)
	}
	return nil
}

func buildHeaders(nodes []*kdl.Node) ([]Header, error) {
	var hs []Header
	seen := map[string]bool{}
	for _, c := range nodes {
		if c.Name != "header" {
			continue
		}
		if len(c.Args) != 2 || len(c.Props) > 0 || len(c.Children) > 0 {
			return nil, fmt.Errorf("header: want header \"Name\" \"value\"")
		}
		name, ok1 := c.Args[0].(string)
		val, ok2 := c.Args[1].(string)
		if !ok1 || !ok2 || !headerRe.MatchString(name) {
			return nil, fmt.Errorf("header: bad name or value")
		}
		low := strings.ToLower(name)
		if low == "host" || low == "content-length" || low == "transfer-encoding" || low == "connection" {
			return nil, fmt.Errorf("header %s cannot be set", name)
		}
		if seen[low] {
			return nil, fmt.Errorf("duplicate header %s", name)
		}
		seen[low] = true
		if strings.ContainsAny(val, "\r\n\x00") {
			return nil, fmt.Errorf("header %s: control characters in value", name)
		}
		if err := checkPlaceholders(val, nil, false, true); err != nil {
			return nil, fmt.Errorf("header %s: %w", name, err)
		}
		if sensitiveHeader.MatchString(name) && len(secretRefs(val)) == 0 {
			return nil, fmt.Errorf("header %s looks like a credential: use a {{secret.NAME}} reference, never an inline value", name)
		}
		hs = append(hs, Header{name, val})
	}
	return hs, nil
}

func buildHTTP(n *kdl.Node, params map[string]*Param) (*HTTP, error) {
	if len(n.Args) > 0 || len(n.Props) > 0 {
		return nil, fmt.Errorf("http: takes only child nodes")
	}
	idx, err := single(n, map[string]bool{"method": false, "url": false, "header": true, "body": false, "timeout": false, "max-output-kb": false})
	if err != nil {
		return nil, err
	}
	h := &HTTP{}
	for _, k := range []string{"method", "url"} {
		if idx[k] == nil {
			return nil, fmt.Errorf("http: %s is required", k)
		}
		if err := noProps(idx[k]); err != nil {
			return nil, err
		}
		if err := noChildren(idx[k]); err != nil {
			return nil, err
		}
	}
	if h.Method, err = strArg(idx["method"]); err != nil {
		return nil, err
	}
	if !methods[h.Method] {
		return nil, fmt.Errorf("method %q must be one of GET POST PUT PATCH DELETE", h.Method)
	}
	if h.URL, err = strArg(idx["url"]); err != nil {
		return nil, err
	}
	if err := checkURL(h.URL, params, true); err != nil {
		return nil, err
	}
	if h.Headers, err = buildHeaders(n.Children); err != nil {
		return nil, err
	}
	if b := idx["body"]; b != nil {
		if err := noProps(b); err != nil {
			return nil, err
		}
		if err := noChildren(b); err != nil {
			return nil, err
		}
		if h.Body, err = strArg(b); err != nil {
			return nil, err
		}
		if len(h.Body) > MaxBody {
			return nil, fmt.Errorf("body larger than %d bytes", MaxBody)
		}
		if h.Method == "GET" || h.Method == "DELETE" {
			return nil, fmt.Errorf("a %s request cannot have a body", h.Method)
		}
		if err := checkPlaceholders(h.Body, params, true, true); err != nil {
			return nil, fmt.Errorf("body: %w", err)
		}
	}
	if h.TimeoutS, err = timeoutOf(idx["timeout"], DefaultHTTPTimeout, MaxHTTPTimeout); err != nil {
		return nil, err
	}
	if h.MaxOutputKB, err = outputOf(idx["max-output-kb"]); err != nil {
		return nil, err
	}
	return h, nil
}

func buildMCPRemote(n *kdl.Node) (*MCPRemote, error) {
	if len(n.Args) > 0 || len(n.Props) > 0 {
		return nil, fmt.Errorf("mcp-remote: takes only child nodes")
	}
	idx, err := single(n, map[string]bool{"url": false, "header": true, "allow-tool": true, "timeout": false})
	if err != nil {
		return nil, err
	}
	m := &MCPRemote{}
	if idx["url"] == nil {
		return nil, fmt.Errorf("mcp-remote: url is required")
	}
	if err := noProps(idx["url"]); err != nil {
		return nil, err
	}
	if err := noChildren(idx["url"]); err != nil {
		return nil, err
	}
	if m.URL, err = strArg(idx["url"]); err != nil {
		return nil, err
	}
	if err := checkURL(m.URL, nil, false); err != nil {
		return nil, err
	}
	if m.Headers, err = buildHeaders(n.Children); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, c := range n.Children {
		if c.Name != "allow-tool" {
			continue
		}
		if err := noProps(c); err != nil {
			return nil, err
		}
		if err := noChildren(c); err != nil {
			return nil, err
		}
		names, err := strArgs(c)
		if err != nil || len(names) == 0 {
			return nil, fmt.Errorf("allow-tool: want one or more tool names")
		}
		for _, t := range names {
			if !toolRe.MatchString(t) {
				return nil, fmt.Errorf("allow-tool: bad tool name %q", t)
			}
			if !seen[t] {
				seen[t] = true
				m.AllowTools = append(m.AllowTools, t)
			}
		}
	}
	if len(m.AllowTools) == 0 {
		return nil, fmt.Errorf("mcp-remote: at least one allow-tool is required (nothing is exposed by default)")
	}
	sort.Strings(m.AllowTools)
	if m.TimeoutS, err = timeoutOf(idx["timeout"], DefaultHTTPTimeout, MaxHTTPTimeout); err != nil {
		return nil, err
	}
	return m, nil
}

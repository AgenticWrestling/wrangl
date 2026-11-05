// Package toolkdl is the model, parser and validator for custom tool definitions written in KDL v2
// (ADR 0003). It only parses and validates; nothing here executes a tool.
package toolkdl

// Effect says what a tool may change. It is the owner's claim and is not verified.
type Effect string

const (
	EffectRead        Effect = "read"
	EffectWrite       Effect = "write"
	EffectDestructive Effect = "destructive"
)

// Kind is the executor kind.
type Kind string

const (
	KindExec      Kind = "exec"
	KindHTTP      Kind = "http"
	KindMCPRemote Kind = "mcp_remote"
)

// Definition is one validated tool.
type Definition struct {
	Name        string
	Description string
	Effect      Effect
	RunKinds    []string // defaults to work, revise
	Params      []Param
	Kind        Kind
	Exec        *Exec
	HTTP        *HTTP
	MCPRemote   *MCPRemote
}

// Param is one declared input.
type Param struct {
	Name        string
	Type        string // string | integer | number | boolean
	Description string
	Required    bool
	Enum        []string
	Pattern     string // anchored RE2 expression (strings only)
	Min, Max    *int64 // integers
	MinLen      int    // strings
	MaxLen      int    // strings; defaults to DefaultMaxLen
	AllowDash   bool   // the value may start with '-' (argv use only)
}

// Exec runs a CLI in the sandbox. Argv elements are either literals or exactly "{{input.x}}".
type Exec struct {
	Argv        []string
	Secrets     []ExecSecret
	Network     string // none | full
	TimeoutS    int
	MaxOutputKB int
}

// ExecSecret exposes the stored secret Name to the process as env var Env (As "env") or as a
// path to a 0600 file in env var Env (As "file"). Only the name is ever in a definition.
type ExecSecret struct {
	Env, Name, As string
}

// HTTP calls a fixed https URL. Headers and Body may contain {{secret.NAME}} placeholders.
type HTTP struct {
	Method      string
	URL         string
	Headers     []Header
	Body        string
	TimeoutS    int
	MaxOutputKB int
}

// Header is one request header.
type Header struct{ Name, Value string }

// MCPRemote makes the controller an MCP client of a Streamable HTTP server.
type MCPRemote struct {
	URL        string
	Headers    []Header
	AllowTools []string
	TimeoutS   int
}

// SecretNames lists the stored secret names the definition refers to (deduplicated, in order).
func (d *Definition) SecretNames() []string {
	var out []string
	seen := map[string]bool{}
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	switch {
	case d.Exec != nil:
		for _, s := range d.Exec.Secrets {
			add(s.Name)
		}
	case d.HTTP != nil:
		for _, h := range d.HTTP.Headers {
			for _, n := range secretRefs(h.Value) {
				add(n)
			}
		}
		for _, n := range secretRefs(d.HTTP.Body) {
			add(n)
		}
	case d.MCPRemote != nil:
		for _, h := range d.MCPRemote.Headers {
			for _, n := range secretRefs(h.Value) {
				add(n)
			}
		}
	}
	return out
}

// Limits.
const (
	DefaultMaxLen      = 1024
	HardMaxLen         = 4096
	MaxParams          = 32
	MaxArgv            = 64
	MaxArgLen          = 4096
	DefaultExecTimeout = 20
	MaxExecTimeout     = 600
	DefaultHTTPTimeout = 20
	MaxHTTPTimeout     = 120
	DefaultOutputKB    = 64
	MaxOutputKB        = 1024
	MaxDescription     = 1000
	MaxBody            = 16 << 10
	MaxPattern         = 256
)

// SecretSpec is one stored secret a tool needs. Env and As are set for exec secrets only; http
// and mcp-remote tools splice {{secret.NAME}} into headers and bodies.
type SecretSpec struct {
	Name string `json:"name"`
	Env  string `json:"env,omitempty"`
	As   string `json:"as,omitempty"`
}

// SecretSpecs lists the secrets the definition needs, in declaration order.
func (d *Definition) SecretSpecs() []SecretSpec {
	if d.Exec != nil {
		var out []SecretSpec
		for _, s := range d.Exec.Secrets {
			out = append(out, SecretSpec{Name: s.Name, Env: s.Env, As: s.As})
		}
		return out
	}
	var out []SecretSpec
	for _, n := range d.SecretNames() {
		out = append(out, SecretSpec{Name: n})
	}
	return out
}

// OutputLimit is the result size cap in bytes (default 64 KB).
func (d *Definition) OutputLimit() int {
	kb := DefaultOutputKB
	switch {
	case d.Exec != nil:
		kb = d.Exec.MaxOutputKB
	case d.HTTP != nil:
		kb = d.HTTP.MaxOutputKB
	}
	if kb <= 0 {
		kb = DefaultOutputKB
	}
	return kb << 10
}

// AsyncAfter is the longest call, in seconds, that is answered in the same request. A tool
// whose timeout is longer runs asynchronously: the call returns a call id at once and the
// agent collects the result with ext_result (ADR 0003; the relay's NATS bound is 30 s).
const AsyncAfter = 25

// TimeoutSeconds is the tool's own per-call limit.
func (d *Definition) TimeoutSeconds() int {
	switch {
	case d.Exec != nil:
		return d.Exec.TimeoutS
	case d.HTTP != nil:
		return d.HTTP.TimeoutS
	case d.MCPRemote != nil:
		return d.MCPRemote.TimeoutS
	}
	return DefaultHTTPTimeout
}

// IsAsync reports whether calls can outlast the synchronous bound.
func (d *Definition) IsAsync() bool { return d.TimeoutSeconds() > AsyncAfter }

// ReservedNames cannot be tool names: ext_<name> would collide with a built-in companion tool.
var ReservedNames = map[string]bool{"result": true}

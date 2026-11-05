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
	DefaultExecTimeout = 30
	MaxExecTimeout     = 600
	DefaultHTTPTimeout = 30
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

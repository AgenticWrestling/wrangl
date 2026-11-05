package toolkdl

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const adrExample = `
tool "deploy_status" {
  description "Show the deployment status of an environment"
  effect "read"
  run-kinds "work" "revise"
  input {
    param "env" type="string" { enum "staging" "prod" }
  }
  exec {
    argv "kubectl" "get" "deploy" "-n" "{{input.env}}" "-o" "json"
    secret "KUBECONFIG" name="kube.prod" as="file"
    network "full"
    timeout 60
  }
}
`

func TestADRExample(t *testing.T) {
	d, err := Parse([]byte(adrExample))
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "deploy_status" || d.Effect != EffectRead || d.Kind != KindExec {
		t.Fatalf("%+v", d)
	}
	if !reflect.DeepEqual(d.RunKinds, []string{"work", "revise"}) {
		t.Fatalf("run kinds %v", d.RunKinds)
	}
	if len(d.Params) != 1 || !reflect.DeepEqual(d.Params[0].Enum, []string{"staging", "prod"}) || !d.Params[0].Required {
		t.Fatalf("params %+v", d.Params)
	}
	x := d.Exec
	if x.Network != "full" || x.TimeoutS != 60 || x.MaxOutputKB != 64 ||
		!reflect.DeepEqual(x.Secrets, []ExecSecret{{"KUBECONFIG", "kube.prod", "file"}}) ||
		!reflect.DeepEqual(x.Argv, []string{"kubectl", "get", "deploy", "-n", "{{input.env}}", "-o", "json"}) {
		t.Fatalf("exec %+v", x)
	}
	if got := d.SecretNames(); !reflect.DeepEqual(got, []string{"kube.prod"}) {
		t.Fatalf("secrets %v", got)
	}
}

func TestExecDefaults(t *testing.T) {
	d, err := Parse([]byte(wrap(`exec { argv "uptime" }`, "")))
	if err != nil {
		t.Fatal(err)
	}
	if d.Exec.Network != "none" || d.Exec.TimeoutS != DefaultExecTimeout {
		t.Fatalf("%+v", d.Exec)
	}
}

func TestHTTPAndMCP(t *testing.T) {
	d, err := Parse([]byte(wrap(`http {
  method "POST"
  url "https://api.example.com/v1/issues/{{input.id}}?q={{input.q}}"
  header "Authorization" "Bearer {{secret.tracker.token}}"
  header "Accept" "application/json"
  body "{\"q\": \"{{input.q}}\"}"
  timeout 20
  max-output-kb 128
}`, `input {
  param "id" type="integer" { min 1; max 99999 }
  param "q" type="string" { max-length 64 }
}`)))
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != KindHTTP || d.HTTP.Method != "POST" || len(d.HTTP.Headers) != 2 || d.HTTP.TimeoutS != 20 || d.HTTP.MaxOutputKB != 128 {
		t.Fatalf("%+v", d.HTTP)
	}
	if got := d.SecretNames(); !reflect.DeepEqual(got, []string{"tracker.token"}) {
		t.Fatalf("secrets %v", got)
	}
	m, err := Parse([]byte(wrap(`mcp-remote {
  url "https://mcp.example.com/mcp"
  header "Authorization" "Bearer {{secret.docs}}"
  allow-tool "search" "fetch"
  allow-tool "search"
}`, "")))
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != KindMCPRemote || !reflect.DeepEqual(m.MCPRemote.AllowTools, []string{"fetch", "search"}) {
		t.Fatalf("%+v", m.MCPRemote)
	}
}

// wrap builds a valid read tool around an executor node and optional input block.
func wrap(executor, input string) string {
	return `tool "t" {
  description "d"
  effect "read"
  run-kinds "work"
  ` + input + `
  ` + executor + `
}`
}

func TestInvalid(t *testing.T) {
	strParam := func(body string) string {
		return wrap(`exec { argv "echo" "{{input.p}}" }`, `input { param "p" type="string" `+body+` }`)
	}
	cases := []struct{ name, src, want string }{
		{"not kdl", `tool "x" {`, "kdl"},
		{"kdl v1 bool", `tool "t" { description "d"; effect "read"; run-kinds "work"; exec { argv "x"; network "full"; flag true } }`, "kdl"},
		{"two tools", adrExample + adrExample, "exactly one"},
		{"other root", `foo "x"`, "exactly one"},
		{"bad name", strings.Replace(adrExample, "deploy_status", "Deploy-Status", 1), "tool name"},
		{"unknown node", wrap(`exec { argv "x" }`, "") + "\n", ""}, // control: valid
		{"unknown top node", strings.Replace(adrExample, `effect "read"`, `effect "read"; sudo #true`, 1), "unknown node"},
		{"dup description", strings.Replace(adrExample, `effect "read"`, `effect "read"; description "x"`, 1), "duplicate"},
		{"missing effect", strings.Replace(adrExample, `effect "read"`, ``, 1), "effect is required"},
		{"bad effect", strings.Replace(adrExample, `"read"`, `"safe"`, 1), "effect"},
		{"no run-kinds", strings.Replace(adrExample, `run-kinds "work" "revise"`, ``, 1), "run-kinds is required"},
		{"bad run kind", strings.Replace(adrExample, `"revise"`, `"root"`, 1), "unknown run kind"},
		{"write in review", strings.Replace(strings.Replace(adrExample, `"read"`, `"write"`, 1), `"revise"`, `"review"`, 1), "read-only"},
		{"destructive in plan", strings.Replace(strings.Replace(adrExample, `"read"`, `"destructive"`, 1), `"revise"`, `"plan"`, 1), "read-only"},
		{"no executor", `tool "t" { description "d"; effect "read"; run-kinds "work" }`, "exactly one of"},
		{"two executors", wrap(`exec { argv "x" }
  http { method "GET"; url "https://a.example.com/" }`, ""), "exactly one of"},
		{"dup param", wrap(`exec { argv "x" }`, `input { param "a" type="boolean"; param "a" type="boolean" }`), "duplicate param"},
		{"bad param type", wrap(`exec { argv "x" }`, `input { param "a" type="object" }`), "type"},
		{"unknown param prop", wrap(`exec { argv "x" }`, `input { param "a" type="string" default="x" }`), "unknown property"},
		{"enum on boolean", wrap(`exec { argv "x" }`, `input { param "a" type="boolean" { enum "x" } }`), "enum"},
		{"min on string", wrap(`exec { argv "x" }`, `input { param "a" type="string" { min 1 } }`), "integer"},
		{"min > max", wrap(`exec { argv "x" }`, `input { param "a" type="integer" { min 5; max 1 } }`), "min > max"},
		{"unanchored pattern", strParam(`{ pattern "[a-z]+" }`), "anchored"},
		{"bad regexp", strParam(`{ pattern "^(a$" }`), "bad pattern"},
		{"undeclared input", wrap(`exec { argv "echo" "{{input.nope}}" }`, ""), "undeclared"},
		{"partial argv", wrap(`exec { argv "echo" "--name={{input.p}}" }`, `input { param "p" type="string" { enum "a" } }`), "whole argv element"},
		{"two in argv", wrap(`exec { argv "echo" "{{input.p}}{{input.p}}" }`, `input { param "p" type="string" { enum "a" } }`), "whole argv element"},
		{"secret in argv", wrap(`exec { argv "echo" "{{secret.x}}" }`, ""), "whole argv element"},
		{"placeholder binary", wrap(`exec { argv "{{input.p}}" }`, `input { param "p" type="string" { enum "a" } }`), "argv[0]"},
		{"dash binary", wrap(`exec { argv "-rf" }`, ""), "argv[0]"},
		{"shell", wrap(`exec { argv "sh" "-c" "echo hi" }`, ""), "argv-only"},
		{"shell path", wrap(`exec { argv "/usr/bin/bash" "-c" "echo hi" }`, ""), "argv-only"},
		{"env launcher", wrap(`exec { argv "env" "X=1" "ls" }`, ""), "argv-only"},
		{"empty argv", wrap(`exec { argv }`, ""), "argv needs"},
		{"no argv", wrap(`exec { network "none" }`, ""), "argv is required"},
		{"free string permits dash", strParam(``), "leading '-'"},
		{"pattern permits dash", strParam(`{ pattern "^[-a-z]+$" }`), "leading '-'"},
		{"enum dash", strParam(`{ enum "a" "-rf" }`), "leading '-'"},
		{"negative int", wrap(`exec { argv "echo" "{{input.p}}" }`, `input { param "p" type="integer" }`), "leading '-'"},
		{"negative min", wrap(`exec { argv "echo" "{{input.p}}" }`, `input { param "p" type="integer" { min -5 } }`), "leading '-'"},
		{"float", wrap(`exec { argv "echo" "{{input.p}}" }`, `input { param "p" type="number" }`), "leading '-'"},
		{"pointless allow-dash", strParam(`allow-dash=#true { enum "a" "b" }`), "already forbid"},
		{"inline secret value", wrap(`exec { argv "x"; secret "TOKEN" "hunter2" name="t" }`, ""), "values are never written"},
		{"inline secret prop", wrap(`exec { argv "x"; secret "TOKEN" name="t" value="hunter2" }`, ""), "unknown property"},
		{"secret no name", wrap(`exec { argv "x"; secret "TOKEN" }`, ""), "name="},
		{"secret bad env", wrap(`exec { argv "x"; secret "token" name="t" }`, ""), "env var"},
		{"secret bad as", wrap(`exec { argv "x"; secret "T" name="t" as="argv" }`, ""), "as must"},
		{"secret dup env", wrap(`exec { argv "x"; secret "T" name="a"; secret "T" name="b" }`, ""), "duplicate secret"},
		{"network lan", wrap(`exec { argv "x"; network "lan" }`, ""), "network"},
		{"timeout 0", wrap(`exec { argv "x"; timeout 0 }`, ""), "timeout"},
		{"timeout huge", wrap(`exec { argv "x"; timeout 100000 }`, ""), "timeout"},
		{"output huge", wrap(`exec { argv "x"; max-output-kb 999999 }`, ""), "max-output-kb"},
		{"exec unknown node", wrap(`exec { argv "x"; shell "yes" }`, ""), "unknown node"},
		{"http plain", wrap(`http { method "GET"; url "http://a.example.com/" }`, ""), "https"},
		{"http scheme ftp", wrap(`http { method "GET"; url "ftp://a.example.com/" }`, ""), "https"},
		{"http host from input", wrap(`http { method "GET"; url "https://{{input.h}}/x" }`, `input { param "h" type="string" { enum "a.example.com" } }`), "host must be literal"},
		{"http host suffix input", wrap(`http { method "GET"; url "https://a.example.com.{{input.h}}/x" }`, `input { param "h" type="string" { enum "evil.com" } }`), "host must be literal"},
		{"http userinfo", wrap(`http { method "GET"; url "https://u:p@a.example.com/" }`, ""), "host must be literal"},
		{"http userinfo trick", wrap(`http { method "GET"; url "https://a.example.com@evil.com/" }`, ""), "host must be literal"},
		{"http backslash", wrap(`http { method "GET"; url "https://a.example.com\\@evil.com/" }`, ""), "host must be literal"},
		{"http no host", wrap(`http { method "GET"; url "https:///x" }`, ""), "host must be literal"},
		{"http secret in url", wrap(`http { method "GET"; url "https://a.example.com/{{secret.k}}" }`, ""), "not allowed"},
		{"http undeclared in url", wrap(`http { method "GET"; url "https://a.example.com/{{input.z}}" }`, ""), "undeclared"},
		{"http bad method", wrap(`http { method "TRACE"; url "https://a.example.com/" }`, ""), "method"},
		{"http get body", wrap(`http { method "GET"; url "https://a.example.com/"; body "x" }`, ""), "cannot have a body"},
		{"http inline auth", wrap(`http { method "GET"; url "https://a.example.com/"; header "Authorization" "Bearer abc" }`, ""), "credential"},
		{"http inline key", wrap(`http { method "GET"; url "https://a.example.com/"; header "X-Api-Key" "abc" }`, ""), "credential"},
		{"http input in header", wrap(`http { method "GET"; url "https://a.example.com/"; header "X-Id" "{{input.p}}" }`, `input { param "p" type="string" { enum "a" } }`), "not allowed"},
		{"http host header", wrap(`http { method "GET"; url "https://a.example.com/"; header "Host" "evil.com" }`, ""), "cannot be set"},
		{"http crlf header", wrap(`http { method "GET"; url "https://a.example.com/"; header "X-A" "a\r\nB: c" }`, ""), "control"},
		{"http dup header", wrap(`http { method "GET"; url "https://a.example.com/"; header "X-A" "1"; header "x-a" "2" }`, ""), "duplicate header"},
		{"http bad placeholder", wrap(`http { method "POST"; url "https://a.example.com/"; body "{{env.HOME}}" }`, ""), "unknown placeholder"},
		{"http unterminated", wrap(`http { method "POST"; url "https://a.example.com/"; body "{{input.p" }`, ""), "malformed"},
		{"http timeout", wrap(`http { method "GET"; url "https://a.example.com/"; timeout 500 }`, ""), "timeout"},
		{"http no url", wrap(`http { method "GET" }`, ""), "url is required"},
		{"mcp http", wrap(`mcp-remote { url "http://m.example.com/"; allow-tool "a" }`, ""), "https"},
		{"mcp no allow", wrap(`mcp-remote { url "https://m.example.com/" }`, ""), "allow-tool"},
		{"mcp input", wrap(`mcp-remote { url "https://m.example.com/"; allow-tool "a" }`, `input { param "a" type="boolean" }`), "no input"},
		{"mcp input in url", wrap(`mcp-remote { url "https://m.example.com/{{input.a}}"; allow-tool "a" }`, ""), "not allowed"},
		{"mcp bad tool", wrap(`mcp-remote { url "https://m.example.com/"; allow-tool "a b" }`, ""), "bad tool name"},
		{"huge description", strings.Replace(adrExample, "Show the deployment status of an environment", strings.Repeat("x", 2000), 1), "description"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.src))
			if c.want == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got none", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q in %q", c.want, err)
			}
		})
	}
}

func TestExplicitAllowDash(t *testing.T) {
	d, err := Parse([]byte(wrap(`exec { argv "echo" "{{input.p}}" }`, `input { param "p" type="integer" allow-dash=#true }`)))
	if err != nil {
		t.Fatal(err)
	}
	if !d.Params[0].AllowDash {
		t.Fatal("allow-dash lost")
	}
	// the same loose param is fine when it never reaches argv
	if _, err := Parse([]byte(wrap(`http { method "GET"; url "https://a.example.com/{{input.p}}" }`, `input { param "p" type="string" }`))); err != nil {
		t.Fatal(err)
	}
}

func TestTightPatternsAccepted(t *testing.T) {
	for _, p := range []string{`^[a-z][a-z0-9-]*$`, `^[0-9]{1,6}$`, `^(staging|prod)$`} {
		src := wrap(`exec { argv "echo" "{{input.p}}" }`, `input { param "p" type="string" { pattern "`+p+`" } }`)
		if _, err := Parse([]byte(src)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(adrExample))
	f.Add([]byte(`tool "x" {`))
	f.Fuzz(func(t *testing.T, b []byte) {
		d, err := Parse(b)
		if err == nil && d == nil {
			t.Fatal("nil definition without error")
		}
	})
}

func TestDeclarationHidesExecutor(t *testing.T) {
	d, err := Parse([]byte(adrExample))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d.Declaration())
	s := string(b)
	for _, leak := range []string{"kubectl", "kube.prod", "KUBECONFIG", "argv", "{{"} {
		if strings.Contains(s, leak) {
			t.Errorf("declaration leaks %q: %s", leak, s)
		}
	}
	for _, want := range []string{`"enum":["staging","prod"]`, `"required":["env"]`, `"run_kinds":["work","revise"]`} {
		if !strings.Contains(s, want) {
			t.Errorf("declaration missing %s: %s", want, s)
		}
	}
}

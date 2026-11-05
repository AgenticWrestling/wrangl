package toolkdl

import (
	"strings"
	"testing"
)

func TestValidateInput(t *testing.T) {
	d, err := Parse([]byte(`tool "t" {
  description "d"
  effect "read"
  run-kinds "work"
  input {
    param "env" type="string" { enum "staging" "prod" }
    param "name" type="string" required=#false { pattern "^[a-z][a-z0-9-]*$"; max-length 12 }
    param "n" type="integer" required=#false { min 1; max 10 }
    param "free" type="string" required=#false allow-dash=#true { max-length 5 }
    param "flag" type="boolean" required=#false
  }
  exec { argv "echo" "{{input.env}}" }
}`))
	if err != nil {
		t.Fatal(err)
	}
	ok := []struct{ in, key, want string }{
		{`{"env":"prod"}`, "env", "prod"},
		{`{"env":"staging","name":"web-1","n":10}`, "n", "10"},
		{`{"env":"prod","free":"-rf"}`, "free", "-rf"},
		{`{"env":"prod","flag":true}`, "flag", "true"},
		{`{"env":"prod","name":null}`, "env", "prod"},
	}
	for _, c := range ok {
		in, err := d.ValidateInput([]byte(c.in))
		if err != nil || in.Args[c.key] != c.want {
			t.Errorf("%s: %v %v", c.in, err, in)
		}
	}
	bad := []struct{ name, in, want string }{
		{"empty", ``, "required"},
		{"not object", `["prod"]`, "JSON object"},
		{"string", `"prod"`, "JSON object"},
		{"trailing", `{"env":"prod"} {"x":1}`, "single"},
		{"missing", `{}`, "required"},
		{"unknown", `{"env":"prod","shell":"id"}`, "unknown argument"},
		{"enum", `{"env":"dev"}`, "one of"},
		{"wrong type", `{"env":5}`, "string"},
		{"dash enum-less", `{"env":"prod","name":"-x"}`, "pattern"},
		{"pattern", `{"env":"prod","name":"Web"}`, "pattern"},
		{"too long", `{"env":"prod","name":"abcdefghijklm"}`, "longer"},
		{"too long free", `{"env":"prod","free":"abcdefg"}`, "longer"},
		{"int min", `{"env":"prod","n":0}`, "at least"},
		{"int max", `{"env":"prod","n":11}`, "at most"},
		{"negative", `{"env":"prod","n":-1}`, "at least"},
		{"float as int", `{"env":"prod","n":1.5}`, "integer"},
		{"string as int", `{"env":"prod","n":"1"}`, "integer"},
		{"bool", `{"env":"prod","flag":"yes"}`, "true or false"},
		{"nul", "{\"env\":\"prod\",\"free\":\"a\\u0000b\"}", "NUL"},
	}
	for _, c := range bad {
		in, err := d.ValidateInput([]byte(c.in))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v %v", c.name, c.want, err, in)
		}
	}
}

func TestLeadingDashRefusedByDefault(t *testing.T) {
	d, err := Parse([]byte(`tool "t" {
  description "d"
  effect "read"
  run-kinds "work"
  input { param "q" type="string" { max-length 20 } }
  http { method "GET"; url "https://a.example.com/search?q={{input.q}}" }
}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ValidateInput([]byte(`{"q":"--output=/etc/passwd"}`)); err == nil || !strings.Contains(err.Error(), "'-'") {
		t.Fatalf("leading dash: %v", err)
	}
	if _, err := d.ValidateInput([]byte(`{"q":"fine-dash"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestErrorsDoNotEchoValues(t *testing.T) {
	d, _ := Parse([]byte(`tool "t" { description "d"; effect "read"; run-kinds "work"; input { param "p" type="string" { enum "a" } }; exec { argv "echo" "{{input.p}}" } }`))
	_, err := d.ValidateInput([]byte(`{"p":"SUPERSECRETVALUE"}`))
	if err == nil || strings.Contains(err.Error(), "SUPERSECRETVALUE") {
		t.Fatalf("error echoes the value: %v", err)
	}
}

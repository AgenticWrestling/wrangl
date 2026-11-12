package apitools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stacktools"
)

// Every route has a valid schema, a unique name that no other tool uses, and every
// {placeholder} in its path is a declared property (or produced by Pre).
func TestCatalogue(t *testing.T) {
	seen := map[string]bool{}
	for _, tl := range stacktools.For("user") {
		seen[tl.Name] = true
	}
	for _, r := range Routes {
		if seen[r.Name] {
			t.Errorf("duplicate tool name %s", r.Name)
		}
		seen[r.Name] = true
		if r.Desc == "" || r.Method == "" || !strings.HasPrefix(r.Path, "/") {
			t.Errorf("%s: incomplete route", r.Name)
		}
		var sch struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		for _, tl := range Tools() {
			if tl.Name == r.Name && json.Unmarshal(tl.InputSchema, &sch) != nil {
				t.Errorf("%s: invalid schema %s", r.Name, tl.InputSchema)
			}
		}
		for _, k := range r.Required {
			if _, ok := sch.Properties[k]; !ok {
				t.Errorf("%s: required %q is not a property", r.Name, k)
			}
		}
		if r.Pre == nil {
			for _, m := range placeholder.FindAllStringSubmatch(r.Path, -1) {
				if _, ok := sch.Properties[m[1]]; !ok {
					t.Errorf("%s: path parameter %q is not a property", r.Name, m[1])
				}
			}
		}
	}
}

func TestSplitRef(t *testing.T) {
	a := map[string]any{"ref": "o/n#12"}
	if err := splitRef(a); err != nil || a["owner"] != "o" || a["name"] != "n" || a["number"] != "12" {
		t.Fatalf("%v %v", a, err)
	}
	if splitRef(map[string]any{"ref": "nope"}) == nil {
		t.Fatal("bad ref accepted")
	}
}

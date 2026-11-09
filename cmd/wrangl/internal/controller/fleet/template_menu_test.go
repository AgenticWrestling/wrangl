package fleet

import (
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/menu"
)

func TestTemplateInstanceMenuKeepsPricesAndLegacyNames(t *testing.T) {
	y := "name: m\narch: arm64\nmin: {vcpu: 2, ram: 4GiB, disk: 20GiB}\ninstance_types:\n  - t4g.medium\n  - { type: c7g.xlarge, price: 0.145 }\n"
	p, err := ParseTemplate([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if p.InstanceTypes != `["t4g.medium",{"type":"c7g.xlarge","price":0.145}]` {
		t.Fatalf("stored: %s", p.InstanceTypes)
	}
	es := menu.Parse(p.InstanceTypes)
	if len(es) != 2 || es[1].Price == nil || strings.Join(menu.Names(p.InstanceTypes), ",") != "t4g.medium,c7g.xlarge" {
		t.Fatalf("%+v", es)
	}
	if _, err := ParseTemplate([]byte(strings.Replace(y, "0.145", "-1", 1))); err == nil {
		t.Fatal("a negative price must be refused")
	}
	// Plain lists are stored exactly as before.
	p, _ = ParseTemplate([]byte("name: m\nmin: {vcpu: 2, ram: 4GiB, disk: 20GiB}\ninstance_types: [a, b]\n"))
	if p.InstanceTypes != `["a","b"]` {
		t.Fatalf("%s", p.InstanceTypes)
	}
}

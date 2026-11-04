package kdl

import "testing"

func TestParse(t *testing.T) {
	ns, err := Parse([]byte(`a "x" 3 #true k="v" { b; c 1.5 }`))
	if err != nil {
		t.Fatal(err)
	}
	n := ns[0]
	if n.Name != "a" || n.Args[0] != "x" || n.Args[1] != int64(3) || n.Args[2] != true || n.Props["k"] != "v" {
		t.Fatalf("%+v", n)
	}
	if len(n.Children) != 2 || n.Children[1].Args[0] != 1.5 {
		t.Fatalf("children %+v", n.Children)
	}
}

func TestParseRejects(t *testing.T) {
	for name, src := range map[string]string{
		"v1 bool":    `a true`,
		"annotation": `a (u8)1`,
		"unclosed":   `a {`,
		"big":        `a 123456789012345678901234567890`,
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := Parse(make([]byte, MaxSize+1)); err == nil {
		t.Error("oversize accepted")
	}
}

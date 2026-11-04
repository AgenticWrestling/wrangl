// Package kdl is the only importer of github.com/njreid/gokdl2. It exposes a minimal, plain-data
// view of a KDL v2 document so the underlying parser can be swapped without touching callers.
package kdl

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"

	gokdl "github.com/njreid/gokdl2"
	"github.com/njreid/gokdl2/document"
)

// MaxSize bounds the accepted document size.
const MaxSize = 64 << 10

// Node is one KDL node. Argument and property values are string, int64, float64, bool or nil.
type Node struct {
	Name     string
	Args     []any
	Props    map[string]any
	Children []*Node
}

// Parse parses a KDL v2 document. Version 1 syntax, type annotations, expressions and big
// numbers are rejected rather than reinterpreted.
func Parse(data []byte) ([]*Node, error) {
	if len(data) > MaxSize {
		return nil, fmt.Errorf("kdl: document larger than %d bytes", MaxSize)
	}
	doc, err := gokdl.ParseWithOptions(bytes.NewReader(data), gokdl.ParseOptions{Version: gokdl.ParseVersionV2})
	if err != nil {
		return nil, fmt.Errorf("kdl: %w", err)
	}
	return convert(doc.Nodes, 0)
}

const maxDepth = 16

func convert(in []*document.Node, depth int) ([]*Node, error) {
	if depth > maxDepth {
		return nil, errors.New("kdl: nesting too deep")
	}
	var out []*Node
	for _, n := range in {
		if n.Name == nil {
			return nil, errors.New("kdl: node without a name")
		}
		if n.Type != "" {
			return nil, fmt.Errorf("kdl: type annotations are not supported (node %q)", name(n))
		}
		o := &Node{Name: name(n)}
		for _, a := range n.Arguments {
			v, err := value(a)
			if err != nil {
				return nil, fmt.Errorf("kdl: node %q: %w", o.Name, err)
			}
			o.Args = append(o.Args, v)
		}
		if n.Properties.Exist() {
			o.Props = map[string]any{}
			for k, pv := range n.Properties.Unordered() {
				v, err := value(pv)
				if err != nil {
					return nil, fmt.Errorf("kdl: node %q property %q: %w", o.Name, k, err)
				}
				o.Props[k] = v
			}
		}
		kids, err := convert(n.Children, depth+1)
		if err != nil {
			return nil, err
		}
		o.Children = kids
		out = append(out, o)
	}
	return out, nil
}

func name(n *document.Node) string { return n.Name.NodeNameStringV2() }

func value(v *document.Value) (any, error) {
	if v.Type != "" {
		return nil, errors.New("type annotations are not supported")
	}
	switch x := v.ResolvedValueV2().(type) {
	case nil, string, bool, int64, float64:
		return x, nil
	case int:
		return int64(x), nil
	case *big.Int, *big.Float:
		return nil, errors.New("number out of range")
	default:
		return nil, fmt.Errorf("unsupported value %T", x)
	}
}

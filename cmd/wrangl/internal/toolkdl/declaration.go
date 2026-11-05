package toolkdl

import "encoding/json"

// Declaration is everything a runner (and so an agent) is told about a tool: no argv, URL,
// header or secret reference. It is what ships in assignments and in the MCP tool list.
type Declaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Effect      Effect          `json:"effect"`
	Kind        Kind            `json:"kind"`
	RunKinds    []string        `json:"run_kinds"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Declaration returns the runner-visible form of d.
func (d *Definition) Declaration() Declaration {
	return Declaration{Name: d.Name, Description: d.Description, Effect: d.Effect, Kind: d.Kind,
		RunKinds: append([]string(nil), d.RunKinds...), InputSchema: d.inputSchema()}
}

func (d *Definition) inputSchema() json.RawMessage {
	props := map[string]any{}
	required := []string{}
	for _, p := range d.Params {
		s := map[string]any{"type": p.Type}
		if p.Description != "" {
			s["description"] = p.Description
		}
		if len(p.Enum) > 0 {
			if p.Type == "integer" {
				var ev []int64
				for _, e := range p.Enum {
					var n int64
					json.Unmarshal([]byte(e), &n)
					ev = append(ev, n)
				}
				s["enum"] = ev
			} else {
				s["enum"] = p.Enum
			}
		}
		if p.Pattern != "" {
			s["pattern"] = p.Pattern
		}
		if p.Min != nil {
			s["minimum"] = *p.Min
		}
		if p.Max != nil {
			s["maximum"] = *p.Max
		}
		if p.Type == "string" {
			if p.MinLen > 0 {
				s["minLength"] = p.MinLen
			}
			s["maxLength"] = p.MaxLen
		}
		props[p.Name] = s
		if p.Required {
			required = append(required, p.Name)
		}
	}
	schema := map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	b, _ := json.Marshal(schema)
	return b
}

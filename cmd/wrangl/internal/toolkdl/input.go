package toolkdl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Input is a call's arguments after validation against the declared params. Args holds each
// value rendered as the single string that fills an argv element or placeholder.
type Input struct {
	Values map[string]any
	Args   map[string]string
}

// ValidateInput checks raw (a JSON object) against the definition's params: unknown keys,
// required, type, enum, pattern, integer bounds and string length. Anything untrusted that
// would render with a leading '-' is refused unless the param declares allow-dash, and NUL or
// other control characters never pass. Errors name the param, never echo the value.
func (d *Definition) ValidateInput(raw json.RawMessage) (*Input, error) {
	in := &Input{Values: map[string]any{}, Args: map[string]string{}}
	var obj map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		if err := dec.Decode(&obj); err != nil || obj == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, fmt.Errorf("arguments must be a JSON object")
		}
		if dec.More() {
			return nil, fmt.Errorf("arguments must be a single JSON object")
		}
	}
	params := map[string]*Param{}
	for i := range d.Params {
		params[d.Params[i].Name] = &d.Params[i]
	}
	for k := range obj {
		if params[k] == nil {
			return nil, fmt.Errorf("unknown argument %q (this tool takes: %s)", k, paramNames(d.Params))
		}
	}
	for i := range d.Params {
		p := &d.Params[i]
		rv, ok := obj[p.Name]
		if !ok || bytes.Equal(bytes.TrimSpace(rv), []byte("null")) {
			if p.Required {
				return nil, fmt.Errorf("argument %q is required", p.Name)
			}
			continue
		}
		val, s, err := checkParam(p, rv)
		if err != nil {
			return nil, fmt.Errorf("argument %q: %w", p.Name, err)
		}
		if strings.HasPrefix(s, "-") && !p.AllowDash {
			return nil, fmt.Errorf("argument %q: a value starting with '-' is not allowed", p.Name)
		}
		in.Values[p.Name], in.Args[p.Name] = val, s
	}
	return in, nil
}

func paramNames(ps []Param) string {
	var n []string
	for _, p := range ps {
		n = append(n, p.Name)
	}
	if len(n) == 0 {
		return "no arguments"
	}
	return strings.Join(n, ", ")
}

func checkParam(p *Param, rv json.RawMessage) (any, string, error) {
	switch p.Type {
	case "string":
		var s string
		if err := json.Unmarshal(rv, &s); err != nil {
			return nil, "", fmt.Errorf("must be a string")
		}
		if strings.ContainsRune(s, 0) {
			return nil, "", fmt.Errorf("contains a NUL byte")
		}
		n := utf8.RuneCountInString(s)
		if n < p.MinLen {
			return nil, "", fmt.Errorf("shorter than %d characters", p.MinLen)
		}
		if n > p.MaxLen {
			return nil, "", fmt.Errorf("longer than %d characters", p.MaxLen)
		}
		if len(p.Enum) > 0 && !contains(p.Enum, s) {
			return nil, "", fmt.Errorf("must be one of: %s", strings.Join(p.Enum, ", "))
		}
		if p.Pattern != "" && !regexp.MustCompile(p.Pattern).MatchString(s) {
			return nil, "", fmt.Errorf("does not match the required pattern %s", p.Pattern)
		}
		return s, s, nil
	case "integer":
		if t := bytes.TrimSpace(rv); len(t) == 0 || t[0] == '"' {
			return nil, "", fmt.Errorf("must be an integer")
		}
		var n json.Number
		dec := json.NewDecoder(bytes.NewReader(rv))
		dec.UseNumber()
		if err := dec.Decode(&n); err != nil {
			return nil, "", fmt.Errorf("must be an integer")
		}
		i, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil {
			return nil, "", fmt.Errorf("must be an integer")
		}
		if p.Min != nil && i < *p.Min {
			return nil, "", fmt.Errorf("must be at least %d", *p.Min)
		}
		if p.Max != nil && i > *p.Max {
			return nil, "", fmt.Errorf("must be at most %d", *p.Max)
		}
		s := strconv.FormatInt(i, 10)
		if len(p.Enum) > 0 && !contains(p.Enum, s) {
			return nil, "", fmt.Errorf("must be one of: %s", strings.Join(p.Enum, ", "))
		}
		return i, s, nil
	case "number":
		var f float64
		if err := json.Unmarshal(rv, &f); err != nil {
			return nil, "", fmt.Errorf("must be a number")
		}
		return f, strconv.FormatFloat(f, 'g', -1, 64), nil
	case "boolean":
		var b bool
		if err := json.Unmarshal(rv, &b); err != nil {
			return nil, "", fmt.Errorf("must be true or false")
		}
		return b, strconv.FormatBool(b), nil
	}
	return nil, "", fmt.Errorf("unsupported type %q", p.Type)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

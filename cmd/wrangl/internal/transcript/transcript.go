// Package transcript normalises Claude Code session JSONL lines into short,
// secret-free entries suitable for publishing and rendering. It is pure: no
// I/O, no NATS.
package transcript

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Entry kinds.
const (
	KindText       = "text"
	KindToolUse    = "tool_use"
	KindToolResult = "tool_result"
	KindThinking   = "thinking"
)

// Truncation limits (runes).
const (
	MaxText    = 8000 // assistant/user prose and thinking
	MaxExcerpt = 400  // tool input / output excerpts
	MaxSummary = 120  // one-line tool summary
)

// Entry is one normalised transcript item.
type Entry struct {
	Seq     int    `json:"seq"`
	TS      string `json:"ts,omitempty"`
	Role    string `json:"role"` // user | assistant
	Kind    string `json:"kind"`
	Text    string `json:"text,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// Normaliser assigns increasing sequence numbers (starting at Next, default 1)
// to entries parsed from successive lines.
type Normaliser struct {
	Next int
	// toolNames maps tool_use id -> tool name so results can carry the tool.
	toolNames map[string]string
}

type line struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// Parse converts one JSONL line into zero or more entries. Malformed or
// unknown lines yield nil.
func (n *Normaliser) Parse(raw []byte) []Entry {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil || len(l.Message) == 0 {
		return nil
	}
	if l.Type != "user" && l.Type != "assistant" {
		return nil
	}
	var m message
	if err := json.Unmarshal(l.Message, &m); err != nil {
		return nil
	}
	role := m.Role
	if role == "" {
		role = l.Type
	}
	if n.Next == 0 {
		n.Next = 1
	}
	var out []Entry
	add := func(e Entry) {
		e.Seq, e.TS, e.Role = n.Next, l.Timestamp, role
		n.Next++
		out = append(out, e)
	}

	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		if t := strings.TrimSpace(s); t != "" {
			add(Entry{Kind: KindText, Text: clip(Redact(t), MaxText)})
		}
		return out
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return nil
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				add(Entry{Kind: KindText, Text: clip(Redact(t), MaxText)})
			}
		case "thinking":
			if t := strings.TrimSpace(b.Thinking); t != "" {
				add(Entry{Kind: KindThinking, Text: clip(Redact(t), MaxText)})
			}
		case "tool_use":
			if n.toolNames == nil {
				n.toolNames = map[string]string{}
			}
			if b.ID != "" {
				n.toolNames[b.ID] = b.Name
			}
			add(Entry{Kind: KindToolUse, Tool: b.Name,
				Summary: Summarise(b.Name, b.Input),
				Text:    clip(Redact(compactJSON(b.Input)), MaxExcerpt)})
		case "tool_result":
			text := strings.TrimSpace(resultText(b.Content))
			tool := n.toolNames[b.ToolUseID]
			sum := tool
			if b.IsError {
				sum += " failed"
			}
			add(Entry{Kind: KindToolResult, Tool: tool,
				Summary: strings.TrimSpace(sum),
				Text:    clip(Redact(text), MaxExcerpt)})
		}
	}
	return out
}

func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []block
	if json.Unmarshal(raw, &parts) == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return ""
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf strings.Builder
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	b, _ := json.Marshal(v)
	buf.Write(b)
	return buf.String()
}

// Summarise builds a one-line description of a tool call, e.g.
// "Bash: go test ./...".
func Summarise(tool string, input json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := in[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	var arg string
	switch tool {
	case "Bash":
		arg = str("command")
	case "Read", "Edit", "Write", "MultiEdit", "NotebookEdit":
		arg = str("file_path", "notebook_path")
	case "Grep", "Glob":
		arg = str("pattern")
	case "WebFetch":
		arg = str("url")
	case "WebSearch":
		arg = str("query")
	case "Task", "Agent":
		arg = str("description")
	default:
		arg = str("command", "file_path", "path", "pattern", "query", "url", "description")
	}
	arg = strings.Join(strings.Fields(Redact(arg)), " ")
	if tool == "" {
		tool = "tool"
	}
	if arg == "" {
		return tool
	}
	return clip(tool+": "+arg, MaxSummary)
}

func clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + fmt.Sprintf("… [+%d chars]", len(r)-max)
}

const redacted = "[REDACTED]"

var (
	// Well-known token shapes.
	reTokens = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{30,}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})\b`)
	reBearer = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{12,}`)
	rePEM    = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`)
	// key=value / key: value / "key":"value" where the key looks secret.
	reKV = regexp.MustCompile(`(?i)(["']?[A-Za-z0-9_.-]*(?:secret|token|passw(?:or)?d|passwd|api[_-]?key|apikey|private[_-]?key|credential|auth)[A-Za-z0-9_.-]*["']?\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s"',;&]+)`)
	// Env dump lines: NAME=value in ALL_CAPS with a secret-ish name (covered
	// by reKV) — also whole `env`/`printenv` style lines with URLs carrying creds.
	reURLCreds = regexp.MustCompile(`(\b[a-z][a-z0-9+.-]*://[^/\s:@]+:)[^@\s/]+(@)`)
)

// Redact masks obvious secrets: private keys, well-known token shapes, bearer
// credentials, URL passwords and key=value pairs whose key names a secret.
func Redact(s string) string {
	if s == "" {
		return s
	}
	s = rePEM.ReplaceAllString(s, redacted)
	s = reTokens.ReplaceAllString(s, redacted)
	s = reBearer.ReplaceAllString(s, "$1 "+redacted)
	s = reURLCreds.ReplaceAllString(s, "${1}"+redacted+"${2}")
	s = reKV.ReplaceAllStringFunc(s, func(m string) string {
		sub := reKV.FindStringSubmatch(m)
		v := sub[2]
		if v == redacted || v == `"`+redacted+`"` {
			return m
		}
		if len(v) > 0 && (v[0] == '"' || v[0] == '\'') {
			return sub[1] + string(v[0]) + redacted + string(v[0])
		}
		return sub[1] + redacted
	})
	return s
}

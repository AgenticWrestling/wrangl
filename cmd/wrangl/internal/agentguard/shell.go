package agentguard

import (
	"fmt"
	"strings"
)

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }

// splitCommands breaks a shell command line into simple commands (argv lists), splitting on
// ; && || | & and newlines outside quotes, and unwrapping $(...) and `...` so a nested command is
// checked too. It is not a shell: it errs toward finding more commands, never fewer.
func splitCommands(line string) [][]string {
	var out [][]string
	var cur []string
	var tok strings.Builder
	have := false
	flushTok := func() {
		if have {
			cur = append(cur, tok.String())
			tok.Reset()
			have = false
		}
	}
	flushCmd := func() {
		flushTok()
		if len(cur) > 0 {
			out = append(out, cur)
			cur = nil
		}
	}
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\\' && i+1 < len(rs):
			i++
			tok.WriteRune(rs[i])
			have = true
		case c == '\'':
			have = true
			for i++; i < len(rs) && rs[i] != '\''; i++ {
				tok.WriteRune(rs[i])
			}
		case c == '"':
			have = true
			for i++; i < len(rs) && rs[i] != '"'; i++ {
				if rs[i] == '\\' && i+1 < len(rs) {
					i++
				}
				if rs[i] == '$' && i+1 < len(rs) && rs[i+1] == '(' {
					inner, end := matchParen(rs, i+1)
					out = append(out, splitCommands(inner)...)
					i = end
					continue
				}
				tok.WriteRune(rs[i])
			}
		case c == '$' && i+1 < len(rs) && rs[i+1] == '(':
			inner, end := matchParen(rs, i+1)
			out = append(out, splitCommands(inner)...)
			i = end
			tok.WriteString("SUBST")
			have = true
		case c == '`':
			j := i + 1
			for j < len(rs) && rs[j] != '`' {
				j++
			}
			out = append(out, splitCommands(string(rs[i+1:min(j, len(rs))]))...)
			i = j
			tok.WriteString("SUBST")
			have = true
		case c == ';' || c == '\n' || c == '|' || c == '&' || c == '(' || c == ')':
			flushCmd()
		case c == ' ' || c == '\t':
			flushTok()
		default:
			tok.WriteRune(c)
			have = true
		}
	}
	flushCmd()
	return out
}

// matchParen returns the text inside the parenthesis that opens at rs[open] and the index of its
// closing one.
func matchParen(rs []rune, open int) (string, int) {
	depth := 0
	for i := open; i < len(rs); i++ {
		switch rs[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return string(rs[open+1 : i]), i
			}
		}
	}
	return string(rs[open+1:]), len(rs)
}

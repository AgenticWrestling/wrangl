package transcript

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T) []Entry {
	t.Helper()
	b, err := os.ReadFile("testdata/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var n Normaliser
	var out []Entry
	for _, l := range bytes.Split(b, []byte("\n")) {
		out = append(out, n.Parse(l)...)
	}
	return out
}

func TestFixture(t *testing.T) {
	es := fixture(t)
	type want struct{ role, kind, tool, summary, contains string }
	wants := []want{
		{"user", KindText, "", "", "run the tests"},
		{"assistant", KindThinking, "", "", "go test first"},
		{"assistant", KindText, "", "", "```go"},
		{"assistant", KindToolUse, "Bash", "Bash: go test ./...", ""},
		{"user", KindToolResult, "Bash", "Bash", "0.01s"},
		{"assistant", KindToolUse, "Read", "Read: /repo/main.go", ""},
		{"user", KindToolResult, "Read", "Read failed", "file not found"},
		{"assistant", KindText, "", "", "<script>"},
	}
	if len(es) != len(wants) {
		t.Fatalf("got %d entries, want %d: %+v", len(es), len(wants), es)
	}
	for i, w := range wants {
		e := es[i]
		if e.Seq != i+1 || e.Role != w.role || e.Kind != w.kind || e.Tool != w.tool || e.Summary != w.summary || !strings.Contains(e.Text, w.contains) {
			t.Errorf("entry %d = %+v, want %+v", i, e, w)
		}
		if e.TS == "" {
			t.Errorf("entry %d missing ts", i)
		}
	}
	if strings.Contains(es[4].Text, "supersecretvalue123") || !strings.Contains(es[4].Text, "API_TOKEN=[REDACTED]") {
		t.Errorf("secret not redacted: %q", es[4].Text)
	}
	if strings.Contains(es[7].Text, "ghp_") {
		t.Errorf("token not redacted: %q", es[7].Text)
	}
}

func TestSkipsBadLines(t *testing.T) {
	var n Normaliser
	for _, l := range []string{"", "{", "[]", `{"type":"summary"}`, `{"type":"user","message":"x"}`,
		`{"type":"user","message":{"content":42}}`, `{"type":"assistant","message":{"content":[{"type":"image"}]}}`} {
		if es := n.Parse([]byte(l)); len(es) != 0 {
			t.Errorf("%q => %+v", l, es)
		}
	}
	if es := n.Parse([]byte(`{"type":"user","message":{"content":"hi"}}`)); len(es) != 1 || es[0].Seq != 1 {
		t.Errorf("seq should not advance on skipped lines: %+v", es)
	}
}

func TestTruncate(t *testing.T) {
	long := strings.Repeat("x", 5000)
	var n Normaliser
	es := n.Parse([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"u","content":"` + long + `"}]}}`))
	if len(es) != 1 || len([]rune(es[0].Text)) > MaxExcerpt+30 || !strings.Contains(es[0].Text, "[+") {
		t.Errorf("not truncated: %d", len(es[0].Text))
	}
	s := Summarise("Bash", []byte(`{"command":"`+long+`"}`))
	if len([]rune(s)) > MaxSummary+30 {
		t.Errorf("summary too long: %d", len(s))
	}
}

func TestRedact(t *testing.T) {
	for _, tc := range []struct{ in, notContain string }{
		{"export AWS_SECRET_ACCESS_KEY=abcd1234efgh", "abcd1234efgh"},
		{`{"password": "hunter2hunter2"}`, "hunter2"},
		{"Authorization: Bearer abcdefghijklmnop1234", "abcdefghijklmnop1234"},
		{"sk-ant-api03-abcdefghijklmnopqrstu", "abcdefghijklmnopqrstu"},
		{"postgres://u:pa55w0rd@host/db", "pa55w0rd"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----", "MIIabc"},
		{"GITHUB_TOKEN='tok123456'", "tok123456"},
	} {
		if got := Redact(tc.in); strings.Contains(got, tc.notContain) || !strings.Contains(got, redacted) {
			t.Errorf("Redact(%q) = %q", tc.in, got)
		}
	}
	if got := Redact("go test ./... -run Foo"); got != "go test ./... -run Foo" {
		t.Errorf("over-redacted: %q", got)
	}
}

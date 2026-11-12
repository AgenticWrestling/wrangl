package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/api"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func TestSessionRows(t *testing.T) {
	now := time.Now()
	rows, managed, unmanaged := sessionRows("w1", "idle", []proto.ClaudeProc{
		{PID: 1, Kind: proto.ClaudeRun, RunID: 7, RSS: 300 << 20, Started: now.Add(-90 * time.Minute)},
		{PID: 2, Kind: proto.ClaudePrimary},
		{PID: 3, Kind: proto.ClaudeOther, RSS: 2 << 30},
	}, now)
	if managed != 2 || unmanaged != 1 {
		t.Fatalf("counts %d/%d", managed, unmanaged)
	}
	if rows[0].Term != "/runner/w1/term#run-7" || rows[0].Age != "1h30m" || rows[0].Mem != "300 MiB" {
		t.Errorf("run row %+v", rows[0])
	}
	if rows[1].Term != "" || rows[2].Term != "" || rows[2].Mem != "2.0 GiB" {
		t.Errorf("other rows %+v %+v", rows[1], rows[2])
	}
	if r, _, _ := sessionRows("w1", "offline", []proto.ClaudeProc{{PID: 1, Kind: proto.ClaudeRun, RunID: 7}}, now); r[0].Term != "" {
		t.Errorf("offline worker got a terminal link")
	}
}

func TestSessionsTemplate(t *testing.T) {
	h, err := New(api.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	rows, m, u := sessionRows("w1", "busy", []proto.ClaudeProc{
		{PID: 11, Kind: proto.ClaudeRun, RunID: 7, Task: "abc123", Cwd: "/work/a"},
		{PID: 12, Kind: proto.ClaudeOther, RCName: "hand", Mux: "zellij:z1"},
	}, time.Now())
	var b bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&b, "sessions", map[string]any{"Sessions": rows, "Managed": m, "Unmanaged": u}); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"1 managed · 1 unmanaged", `href="/runner/w1/term#run-7"`, "unmanaged", "hand", "zellij:z1", `href="/task/abc123"`} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("missing %q in %s", w, b.String())
		}
	}
	b.Reset()
	h.tmpl.ExecuteTemplate(&b, "sessions", map[string]any{})
	if strings.TrimSpace(b.String()) != "" {
		t.Errorf("no sessions should render nothing: %q", b.String())
	}
}

func TestRunnersRedirectsToWorkers(t *testing.T) {
	h, err := New(api.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/runners", nil))
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/workers" {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSessionPillAndButtonTemplates(t *testing.T) {
	h, err := New(api.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	render := func(name string, b *sessionBtn) string {
		var buf bytes.Buffer
		if err := h.tmpl.ExecuteTemplate(&buf, name, b); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	run := &sessionBtn{State: "running", Health: "running, pid 7, up 2m", Href: "/runner/host/term#primary-alpha"}
	if p := render("sesspill", run); !strings.Contains(p, "sess-running") || !strings.Contains(p, "session running") || !strings.Contains(p, "pid 7") {
		t.Errorf("pill %s", p)
	}
	if b := render("sessbtn", run); !strings.Contains(b, `href="/runner/host/term#primary-alpha"`) || !strings.Contains(b, ">Session<") {
		t.Errorf("button %s", b)
	}
	none := &sessionBtn{State: "none"}
	if b := render("sessbtn", none); strings.TrimSpace(b) != "" {
		t.Errorf("a session with nothing to open still has a button: %s", b)
	}
	if p := render("sesspill", &sessionBtn{State: "restarting", Health: "ended (exited with code 1); restarting in 5s"}); !strings.Contains(p, "sess-restarting") {
		t.Errorf("pill %s", p)
	}
}

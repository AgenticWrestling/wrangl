package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// checkPWA drives the M8 web UI against a controller that has run the PR and plan flows.
func checkPWA(t *testing.T, e *workEnv, hc *http.Client, runner string, big, other string, planQ int64) {
	t.Helper()
	get := func(path string, want ...string) string {
		t.Helper()
		resp, err := hc.Get("http://x" + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: %d %.400s", path, resp.StatusCode, b)
		}
		for _, w := range want {
			if !strings.Contains(string(b), w) {
				t.Fatalf("GET %s: missing %q in %.2000s", path, w, b)
			}
		}
		return string(b)
	}
	post := func(path, signals string) string {
		t.Helper()
		resp, err := hc.Post("http://x"+path, "application/json", strings.NewReader(signals))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}

	get("/backlog?project=web", `id="main"`, "big job", `data-sqid="`+big+`"`, "sheet-new", `\/ui\/live?`, "datastar-1.0.0-beta.11.js")
	get("/task/"+big, "design it", "sheet-split", "Plan with agent", "do the whole thing")
	get(fmt.Sprintf("/task/%s?split=%d#split", other, planQ), "data-signals-outline=")
	get("/runners", runner, "tiny")
	get("/runner/"+runner, "Cordon", "Recent runs")
	get("/runs", "recent", "small feature")
	get("/run/1", `id="term"`, "xterm-6.0.0.js", "Branch")
	get("/run/1/diff")
	get("/run/1/files")
	get("/inbox", "plan_review", "another big job", `pre class="outline"`)
	get("/more", "enablePush", "tiny", "sheet-project")
	get("/static/app.js", "moveTask")
	get("/static/chroma.css", ".chroma")
	get("/sw.js", "notificationclick")
	get("/manifest.webmanifest", `"standalone"`)
	if png := get("/icons/192.png"); !strings.HasPrefix(png, "\x89PNG") {
		t.Fatalf("icon is not a PNG")
	}

	// The live stream opens with a morph of #main and the inbox badge.
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://x/ui/live?view=backlog&project=web", nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var live strings.Builder
	for sc.Scan() {
		live.WriteString(sc.Text() + "\n")
		if strings.Contains(sc.Text(), "inbox-badge") {
			break
		}
	}
	resp.Body.Close()
	if s := live.String(); !strings.Contains(s, "event: datastar-merge-fragments") || !strings.Contains(s, `data: fragments <main id="main">`) || !strings.Contains(s, "big job") {
		t.Fatalf("live stream: %.1500s", s)
	}

	// Actions: create (signals as JSON), split, and a failing action's error toast.
	if out := post("/ui/act/task-new?project=web", `{"title":"from the phone","prompt":"tap tap","ttype":"docs"}`); !strings.Contains(out, "created ") || !strings.Contains(out, "datastar-merge-signals") {
		t.Fatalf("task-new: %s", out)
	}
	var sqid string
	bl, _ := e.api.Tasks(e.ctx, "web", "", "")
	for _, n := range bl {
		if n.Title == "from the phone" {
			sqid = n.Sqid
		}
	}
	if sqid == "" {
		t.Fatalf("created task not in backlog: %+v", bl)
	}
	if d := e.task(sqid); d.Type != "docs" {
		t.Fatalf("task-new type: %q", d.Type)
	}
	if out := post("/ui/act/task-edit?sqid="+sqid, `{"etitle":"from the phone","eprompt":"tap tap","etype":"review"}`); !strings.Contains(out, "saved") {
		t.Fatalf("task-edit: %s", out)
	}
	if d := e.task(sqid); d.Type != "review" {
		t.Fatalf("task-edit type: %q", d.Type)
	}
	get("/backlog?project=web", `id="ti-review"`, `class="ticon ti-review"`)
	if out := post("/ui/act/task-split?sqid="+sqid, `{"outline":"- one\n- two\n  - two.a","chain":true}`); !strings.Contains(out, "split into 2") {
		t.Fatalf("split: %s", out)
	}
	if d := e.task(sqid); len(d.Children) != 2 || d.Children[1].Title != "two" {
		t.Fatalf("split result: %+v", d.Children)
	}
	if out := post("/ui/act/task-queue?sqid=nope", `{}`); !strings.Contains(out, "toast show err") {
		t.Fatalf("error toast: %s", out)
	}
}

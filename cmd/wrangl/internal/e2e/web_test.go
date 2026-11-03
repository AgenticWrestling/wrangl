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
	get("/task/"+big, "design it", "sheet-split", "Plan with agent", "do the whole thing", `id="fa-queue"`, `id="subtasks"`, `<code class="sqid">`)
	get(fmt.Sprintf("/task/%s?split=%d#split", other, planQ), "data-signals-outline=")
	get("/ui/detail?sqid="+big, "design it", `id="subtasks"`)
	get("/ui/detail?sqid="+big+"&part=sheets", "sheet-edit", `data-bind="etarget"`, "Target date")

	// A page that expanded a task keeps it across live re-renders: the stream includes the card.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://x/ui/live?view=backlog&project=web&sid=testsid", nil)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		rd := bufio.NewReader(resp.Body)
		readFrame := func() string { // one SSE event
			var b strings.Builder
			for {
				line, err := rd.ReadString('\n')
				if err != nil {
					return b.String()
				}
				if line == "\n" && b.Len() > 0 {
					return b.String()
				}
				b.WriteString(line)
			}
		}
		if first := readFrame(); strings.Contains(first, `id="subtasks"`) {
			t.Fatalf("nothing is expanded yet, but the card is in the stream: %.300s", first)
		}
		pr, _ := hc.Post("http://x/ui/expand?sid=testsid&sqid="+big, "text/plain", nil)
		pr.Body.Close()
		var got string
		for i := 0; i < 4 && !strings.Contains(got, `id="subtasks"`); i++ {
			got = readFrame()
		}
		if !strings.Contains(got, `id="subtasks"`) || !strings.Contains(got, `id="d-`+big+`"`) {
			t.Fatalf("the live re-render does not include the expanded card: %.400s", got)
		}
		cancel()
		resp.Body.Close()
	}
	post("/ui/act/task-edit?sqid="+big, `{"etitle":"design it","eprompt":"do the whole thing","etype":"milestone","etarget":"2026-12-31"}`)
	get("/task/"+big, "2026-12-31", "Milestone")
	get("/backlog?project=web", "ty-milestone", `id="d-`+big+`"`, "2026-12-31")
	get("/static/app.css", ".ty-milestone > .row .title", "font-weight: 600")
	get("/runners", runner, "tiny")
	get("/runner/"+runner, "Cordon", "Recent runs")
	get("/runner/"+runner+"/term", `id="termapp"`, `href="/runner/`+runner+`"`, "wrangl</a>", `/static/vendor/wterm-0.5.7.js`, `/static/term.js`, `/static/term.css`)
	get("/static/vendor/wterm-0.5.7.js", "WTerm")
	get("/static/vendor/wterm-0.5.7.css", ".wterm")
	get("/static/term.js", "Engine", "/term/")
	get("/static/term.css", ".tkeys", "safe-area-inset")
	get("/runs", "recent", "small feature")
	get("/run/1", `id="term"`, "xterm-6.0.0.js", "Branch")
	get("/run/1/diff")
	get("/run/1/files")
	get("/inbox", "plan_review", "another big job", `pre class="outline"`)
	get("/settings", "enablePush", "tiny", "sheet-project", "Appearance", `id="fs-seg"`, `data-fs="1.15"`)
	get("/static/app.css", "calc(16px * var(--fs, 1))")
	get("/backlog?project=web", `class="projbar"`, `aria-label="New project"`, "sheet-project", "/project/web")
	post("/ui/act/project-new", `{"slug":"goalproj","pname":"Goal project","pgoals":"## Done when\n- it ships"}`)
	get("/project/goalproj", "Goal project", "Done when", "it ships", "Custom tools", "coming soon", "sheet-repo", `id="repos"`)
	post("/ui/act/project-save?slug=goalproj", `{"pname":"Renamed","pgoals":"new **goals**","ptemplate":"","pautomerge":false,"pautoqueue":true}`)
	get("/project/goalproj", "Renamed", "<strong>goals</strong>", "auto-queue")
	get("/static/vendor/fonts/red-hat-display-latin.woff2")
	get("/static/app.css", "Red Hat Display", "min-width: 900px")
	get("/static/vendor/fonts/ibm-plex-mono-latin.woff2")
	get("/static/app.css", "IBM Plex Mono", "--sqid")
	get("/setup", "GitHub accounts", "Reviewer App", "Add account")
	// Accounts: a GitHub App lives in the account that owns the repositories, personal or org.
	post("/ui/act/account-add", `{"acctlogin":"plank-pattern","acctkind":"org"}`)
	post("/ui/act/account-add", `{"acctlogin":"@njreid","acctkind":"user"}`)
	setup := get("/setup", "plank-pattern", "organisation", "njreid", "personal", "no App yet")
	if !strings.Contains(setup, "https://github.com/organizations/plank-pattern/settings/apps/new?state=") ||
		!strings.Contains(setup, "https://github.com/settings/apps/new?state=") {
		t.Fatalf("the org and personal App forms must post to different GitHub URLs: %.1500s", setup)
	}
	get("/setup?account=somebody-else&repo=somebody-else/x&back=/project/web", "somebody-else is not set up yet", "somebody-else/x")
	get("/settings", "GitHub accounts", "plank-pattern", "set up the App")
	if out := post("/ui/act/repo-link?slug=web", `{"repoadd":"somebody-else/missing"}`); !strings.Contains(out, "/setup?account=") {
		t.Fatalf("linking a repo the App cannot see must point at the fix: %s", out)
	}
	post("/ui/act/account-remove?login=plank-pattern", `{}`)
	get("/setup/github/callback?code=x&state=bad", "expired or was already used")
	get("/settings", `href="/setup"`)
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

package e2e

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/tools"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/toolkdl"
)

const toolSecret = "tok-LIVE-9c41f7a2e5"

// toolAgent is a fake agent that uses the project's custom tools through the daemon socket,
// the same path the MCP shim takes (socket -> daemon -> NATS -> controller). It prints every
// result so the test can read them from the run log.
const toolAgent = `#!/bin/sh
tool() { curl -s --unix-socket "$WRANGL_SOCK" -d "$2" "http://x/tool/$1"; }
echo "fake agent starting"
LIST=$(curl -s --unix-socket "$WRANGL_SOCK" http://x/tools)
echo "TOOLS: $(printf '%s' "$LIST" | grep -o '"name":"ext_[a-z_]*"' | tr '\n' ' ')"
echo "TOOLS_LEAK: $(printf '%s' "$LIST" | grep -c -e '127.0.0.1' -e 'Bearer' -e 'argv' -e 'secret')"
echo "R1: $(tool ext_lookup '{"id":"abc","q":"hello world"}')"
echo "R2: $(tool ext_lookup '{"id":"..","q":"x"}')"
echo "R3: $(tool ext_lookup '{"id":"abc","q":"--output=x"}')"
echo "R4: $(tool ext_lookup '{"id":"abc","q":"x","admin":"true"}')"
echo "R5: $(tool ext_big '{}')"
echo "R6: $(tool ext_elsewhere '{}')"
echo "R7: $(tool ext_create '{"title":"t\",\"admin\":true,\"x\":\"","count":2}')"
for i in $(seq 1 150); do
  r=$(tool ext_wipe '{"env":"staging"}')
  case "$r" in *pending_approval*) sleep 0.1;; *) break;; esac
done
echo "R8: $r"
tool task_complete "{\"sqid\":\"$WRANGL_TASK\",\"summary\":\"did $WRANGL_TASK\"}" >/dev/null
echo "fake agent finished"
`

type upstreamReq struct {
	Method, Path, Query, Auth, Body string
}

const lookupKDL = `tool "lookup" {
  description "Look up an item"
  effect "read"
  run-kinds "work" "review"
  input {
    param "id" type="string" { max-length 40 }
    param "q" type="string" required=#false { max-length 80 }
  }
  http {
    method "GET"
    url "https://%[1]s/v1/items/{{input.id}}?q={{input.q}}"
    header "Authorization" "Bearer {{secret.upstream.token}}"
    max-output-kb 1
  }
}`

const bigKDL = `tool "big" {
  description "Returns a lot"
  effect "read"
  run-kinds "work"
  http {
    method "GET"
    url "https://%[1]s/v1/big"
    header "Authorization" "Bearer {{secret.upstream.token}}"
    max-output-kb 1
  }
}`

const createKDL = `tool "create" {
  description "Create a thing"
  effect "write"
  run-kinds "work"
  input {
    param "title" type="string" { max-length 100 }
    param "count" type="integer" { min 1; max 50 }
  }
  http {
    method "POST"
    url "https://%[1]s/v1/things"
    header "Authorization" "Bearer {{secret.upstream.token}}"
    body "{\"title\": \"{{input.title}}\", \"count\": \"{{input.count}}\"}"
  }
}`

const wipeKDL = `tool "wipe" {
  description "Wipe an environment"
  effect "destructive"
  run-kinds "work"
  input { param "env" type="string" { enum "staging" "prod" } }
  http {
    method "DELETE"
    url "https://%[1]s/v1/envs/{{input.env}}"
    header "Authorization" "Bearer {{secret.upstream.token}}"
  }
}`

const elsewhereKDL = `tool "elsewhere" {
  description "Enabled only on another project"
  effect "read"
  run-kinds "work"
  http {
    method "GET"
    url "https://%[1]s/v1/elsewhere"
    header "Authorization" "Bearer {{secret.upstream.token}}"
  }
}`

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// TestCustomToolsEndToEnd defines http tools in KDL, adds them, sets the secret, enables them
// and has an agent call them through daemon -> NATS -> controller against a fake upstream.
// It checks results, the audit rows, the size cap, scrubbing, scope and effect enforcement, and
// that neither the secret nor any argv/URL template reaches the runner or any log.
func TestCustomToolsEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs linux (PTY, controller-served runner binary)")
	}
	var (
		mu   sync.Mutex
		reqs []upstreamReq
	)
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, upstreamReq{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Authorization"), string(b)})
		mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/items/"):
			// an upstream that helpfully echoes the credential it was given
			fmt.Fprintf(w, `{"path":%q,"q":%q,"echo_auth":%q}`, r.URL.Path, r.URL.Query().Get("q"), r.Header.Get("Authorization"))
		case r.URL.Path == "/v1/big":
			fmt.Fprint(w, "debug token="+toolSecret+" ", strings.Repeat("A", 200<<10))
		case r.URL.Path == "/v1/things":
			fmt.Fprint(w, "created")
		case strings.HasPrefix(r.URL.Path, "/v1/envs/"):
			fmt.Fprint(w, "wiped "+strings.TrimPrefix(r.URL.Path, "/v1/envs/"))
		default:
			http.NotFound(w, r)
		}
	}))
	up.TLS = &tls.Config{}
	up.StartTLS()
	defer up.Close()
	host := strings.TrimPrefix(up.URL, "https://") // 127.0.0.1:PORT
	seen := func() []upstreamReq { mu.Lock(); defer mu.Unlock(); return append([]upstreamReq(nil), reqs...) }

	var ctlLog, runnerLog syncBuf
	var assignMu sync.Mutex
	var assignments [][]byte
	h := newAgentHarness(t, toolAgent, `name: tools
arch: %s
min: {vcpu: 1, ram: 256MiB, disk: 1GiB}
max_sessions: 1
mise_toml: ""
agent:
  start: [%q, "{{.Prompt}}"]
`, nil,
		harnessOpt{cfgLog: &ctlLog, controller: func(srv *controller.Server) {
			// the production executor refuses loopback and verifies certificates; the fake
			// upstream needs the test-only variant
			srv.ToolExecutors = map[toolkdl.Kind]tools.Executor{toolkdl.KindHTTP: tools.NewLoopbackHTTPExecutor()}
		}},
		harnessOpt{daemon: func(o *runner.DaemonOptions) {
			o.Log = slogTo(&runnerLog)
			o.OnAssign = func(raw []byte) { assignMu.Lock(); assignments = append(assignments, raw); assignMu.Unlock() }
		}})
	defer h.stop()
	ctx, api := h.ctx, h.api

	// 1. define, add, secret, enable (through the same client the CLI uses)
	for _, src := range []string{lookupKDL, bigKDL, createKDL, wipeKDL, elsewhereKDL} {
		if _, err := api.AddTool(ctx, fmt.Sprintf(src, host)); err != nil {
			t.Fatalf("add tool: %v", err)
		}
	}
	if _, err := api.AddTool(ctx, `tool "bad" { description "d"; effect "read"; run-kinds "work"; exec { argv "sh" "-c" "id" } }`); err == nil {
		t.Fatal("a shell tool must be refused at add time")
	}
	if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: "other", Template: ptr("tools")}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"lookup", "big", "create", "wipe"} {
		if _, err := api.EnableTool(ctx, "web", n, true); err != nil {
			t.Fatalf("enable %s: %v", n, err)
		}
	}
	if _, err := api.EnableTool(ctx, "other", "elsewhere", true); err != nil {
		t.Fatal(err)
	}
	en, err := api.EnableTool(ctx, "web", "lookup", true)
	if err != nil || len(en.Secrets) != 1 || en.Secrets[0].Set {
		t.Fatalf("before the secret is set it must read as missing: %+v %v", en, err)
	}
	if err := api.SetToolSecret(ctx, "web", "upstream.token", toolSecret); err != nil {
		t.Fatal(err)
	}
	if st, err := api.ToolSecrets(ctx, "web", "lookup"); err != nil || len(st) != 1 || !st[0].Set {
		t.Fatalf("secret status: %+v %v", st, err)
	}
	if st, _ := api.ToolSecrets(ctx, "other", "elsewhere"); len(st) != 1 || st[0].Set {
		t.Fatalf("the secret is per project: %+v", st)
	}

	// 2. an owner test call (no run) goes through the engine too
	res, err := api.TestTool(ctx, "web", "lookup", json.RawMessage(`{"id":"zed"}`))
	if err != nil || !strings.Contains(res.Output, `"path":"/v1/items/zed"`) || strings.Contains(res.Output, toolSecret) {
		t.Fatalf("tool test: %+v %v", res, err)
	}

	// 3. the agent run
	parent := h.mk(proto.TaskCreateRequest{Title: "use the tools"})
	runID, _, err := api.QueueTask(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	// the destructive call asks the owner; approve it when the question appears
	approved := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			qs, _ := api.Questions(ctx, false)
			for _, q := range qs {
				if q.Kind == "approval" && strings.Contains(q.Prompt, "ext_wipe") {
					approved <- api.Answer(ctx, q.ID, "approve", "")
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		approved <- fmt.Errorf("no approval question for ext_wipe appeared")
	}()
	v := h.waitRun(runID, "succeeded", "failed", "blocked")
	text := h.runLog(runID)
	if v.State != "succeeded" {
		t.Fatalf("run: %+v\n%s", v, text)
	}
	if err := <-approved; err != nil {
		t.Fatal(err)
	}
	line := func(tag string) string {
		for _, l := range strings.Split(text, "\n") {
			if i := strings.Index(l, tag+": "); i >= 0 {
				return l[i+len(tag)+2:]
			}
		}
		t.Fatalf("no %s line in the log:\n%s", tag, text)
		return ""
	}

	// what the agent was offered: enabled tools of its project only, declarations only
	tl := line("TOOLS")
	for _, want := range []string{"ext_lookup", "ext_big", "ext_create", "ext_wipe"} {
		if !strings.Contains(tl, want) {
			t.Errorf("tool list lacks %s: %s", want, tl)
		}
	}
	if strings.Contains(tl, "ext_elsewhere") || strings.Contains(tl, "ext_result") {
		t.Errorf("tool list: %s", tl)
	}
	if line("TOOLS_LEAK") != "0" {
		t.Errorf("the daemon's tool list carries argv, URL or secret text")
	}

	// R1: a real call, secret used upstream, never shown to the agent
	r1 := line("R1")
	if !strings.Contains(r1, `\"path\":\"/v1/items/abc\"`) && !strings.Contains(r1, `"path":"/v1/items/abc"`) {
		t.Errorf("R1: %s", r1)
	}
	if strings.Contains(r1, toolSecret) || !strings.Contains(r1, "[REDACTED]") {
		t.Errorf("R1 must show the echoed credential scrubbed: %s", r1)
	}
	// R2..R4: hostile arguments are refused before any request is made
	for tag, want := range map[string]string{"R2": "dot segment", "R3": "'-'", "R4": "unknown argument"} { // R2 is refused while building the URL
		if r := line(tag); !strings.Contains(r, want) || !strings.Contains(r, `"error"`) {
			t.Errorf("%s: want error containing %q, got %s", tag, want, r)
		}
	}
	// R5: size cap and scrubbing of a 200 KB answer with a token in it
	r5 := line("R5")
	if strings.Contains(r5, toolSecret) || !strings.Contains(r5, "truncated") || len(r5) > 2200 {
		t.Errorf("R5 (cap/scrub): len=%d %.200s", len(r5), r5)
	}
	// R6: scope: a tool enabled on another project does not exist for this run. The daemon
	// refuses a name the run was not given before it costs a round trip; the controller's own
	// check is exercised below.
	if r := line("R6"); !strings.Contains(r, "unknown tool ext_elsewhere") {
		t.Errorf("R6: %s", r)
	}
	// R7: the JSON body is built, not concatenated
	if r := line("R7"); !strings.Contains(r, "created") {
		t.Errorf("R7: %s", r)
	}
	var create *upstreamReq
	for _, q := range seen() {
		if q.Path == "/v1/things" {
			q := q
			create = &q
		}
	}
	if create == nil {
		t.Fatal("the create call never reached the upstream")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(create.Body), &body); err != nil || len(body) != 2 || body["title"] != `t","admin":true,"x":"` || body["count"] != float64(2) { // typed: a whole placeholder keeps the integer
		t.Errorf("injection: upstream saw body %s (%v)", create.Body, err)
	}
	// R8: the destructive call ran only after the owner approved
	if r := line("R8"); !strings.Contains(r, "wiped staging") {
		t.Errorf("R8: %s", r)
	}
	wipes := 0
	for _, q := range seen() {
		if q.Method == "DELETE" {
			wipes++
		}
	}
	if wipes != 1 {
		t.Errorf("the destructive tool reached the upstream %d times, want exactly once after approval", wipes)
	}
	// the secret reached the upstream as the credential
	for _, q := range seen() {
		if q.Auth != "Bearer "+toolSecret {
			t.Errorf("upstream request %s %s carried auth %q", q.Method, q.Path, q.Auth)
		}
	}
	// the refused calls made no upstream requests: 1 test + R1 + R5 + R7 + wipe
	if n := len(seen()); n != 5 {
		t.Errorf("upstream saw %d requests, want 5 (hostile and out-of-scope calls must not reach it)", n)
	}

	// 4. audit: one row per call, with the right statuses
	rows, err := h.db.RQ.ListToolCallsForRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.Tool+":"+r.Status)
	}
	want := []string{"ext_lookup:ok", "ext_lookup:error", "ext_lookup:invalid", "ext_lookup:invalid", "ext_big:ok",
		"ext_create:ok", "ext_wipe:pending_approval", "ext_wipe:ok"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("audit rows:\n got %v\nwant %v", got, want)
	}
	for _, r := range rows {
		if r.Tool == "ext_big" && (r.Bytes <= 0 || r.Bytes > 1500) {
			t.Errorf("ext_big audit bytes = %d, want the capped size", r.Bytes)
		}
	}

	// 5. effect and scope enforced by the controller itself, whatever a runner claims
	sc, err := h.srv.Runs.ScopeForRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	review := sc
	review.Kind = "review"
	for _, name := range []string{"ext_create", "ext_wipe", "ext_big"} { // write, destructive, and a work-only read tool
		if _, err := h.srv.Runs.ExecTool(ctx, review, name, json.RawMessage(`{}`)); !isForbidden(err) {
			t.Errorf("review run calling %s: %v", name, err)
		}
	}
	if out, err := h.srv.Runs.ExecTool(ctx, review, "ext_lookup", json.RawMessage(`{"id":"r"}`)); err != nil || out == nil {
		t.Errorf("a read tool listing review is allowed: %v %v", out, err)
	}
	// a tool enabled only on another project is not available to this run, and the attempt is audited
	if _, err := h.srv.Runs.ExecTool(ctx, sc, "ext_elsewhere", json.RawMessage(`{}`)); !isForbidden(err) {
		t.Errorf("a tool of another project: %v", err)
	}
	if rows, _ := h.db.RQ.ListToolCallsForRun(ctx, runID); len(rows) == 0 || rows[len(rows)-1].Tool != "ext_elsewhere" || rows[len(rows)-1].Status != "denied" {
		t.Errorf("the denied call must be audited: %+v", rows)
	}
	other := sc
	other.ProjectID = 0
	if _, err := h.srv.Runs.ExecTool(ctx, other, "ext_lookup", json.RawMessage(`{"id":"r"}`)); !isForbidden(err) {
		t.Errorf("a scope without a project: %v", err)
	}
	if _, err := h.srv.Runs.ExecTool(ctx, runs.UserScope(), "ext_lookup", json.RawMessage(`{"id":"r"}`)); !isForbidden(err) {
		t.Errorf("the owner's remote MCP has no run: %v", err)
	}

	// 6. nothing sensitive reached the runner, its log, the controller log or any run log
	assignMu.Lock()
	defer assignMu.Unlock()
	if len(assignments) == 0 {
		t.Fatal("the runner saw no assignment")
	}
	u, _ := url.Parse(up.URL)
	secrets := []string{toolSecret, "upstream.token", "Bearer"}
	templates := []string{up.URL, u.Host + "/v1", "/v1/items", "/v1/big", "/v1/things", "/v1/envs", "{{secret", "argv", "kubectl"}
	for i, a := range assignments {
		var asg proto.Assignment
		if err := json.Unmarshal(a, &asg); err != nil {
			t.Fatal(err)
		}
		for _, bad := range append(append([]string{}, secrets...), templates...) {
			if strings.Contains(string(a), bad) {
				t.Errorf("assignment %d contains %q:\n%s", i, bad, a)
			}
		}
		names := ""
		for _, d := range asg.Tools {
			names += d.Name + " "
			if len(d.InputSchema) == 0 {
				t.Errorf("declaration %s has no schema", d.Name)
			}
		}
		if i == 0 && (!strings.Contains(names, "ext_lookup") || strings.Contains(names, "ext_elsewhere")) {
			t.Errorf("assignment tools: %s", names)
		}
	}
	for name, log := range map[string]string{"controller log": ctlLog.String(), "runner log": runnerLog.String(), "run log": text} {
		if strings.Contains(log, toolSecret) {
			t.Errorf("the %s contains the secret value", name)
		}
	}
	for name, log := range map[string]string{"controller log": ctlLog.String(), "runner log": runnerLog.String()} {
		for _, bad := range []string{up.URL, "{{secret", "Bearer"} {
			if strings.Contains(log, bad) {
				t.Errorf("the %s contains %q", name, bad)
			}
		}
	}
	// and no API response gives the value back
	for _, fn := range []func() (any, error){
		func() (any, error) { return api.Tools(ctx) },
		func() (any, error) { return api.Tool(ctx, "lookup") },
		func() (any, error) { return api.ProjectTools(ctx, "web") },
		func() (any, error) { return api.ToolSecrets(ctx, "web", "lookup") },
		func() (any, error) { return api.Secrets(ctx) },
	} {
		v, err := fn()
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := json.Marshal(v); strings.Contains(string(b), toolSecret) {
			t.Errorf("an API response contains the secret value: %s", b)
		}
	}
}

func isForbidden(err error) bool {
	return err != nil && strings.Contains(err.Error(), errs.Forbidden.Error())
}

func slogTo(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

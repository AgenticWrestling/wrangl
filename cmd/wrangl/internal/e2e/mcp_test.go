package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wcli "github.com/agenticwrestling/wrangl/cmd/wrangl/internal/cli"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/client"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/config"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// TestRemoteMCP drives the controller's /mcp endpoint (M10) the way the owner's own agent
// would: directly over HTTP, and through the `wrangl mcp` stdio bridge.
func TestRemoteMCP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if _, err := controller.Init(ctx, data, filepath.Join(dir, "c.yaml")); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultController()
	cfg.DataDir = data
	cfg.Listen = config.Listen{HTTP: "127.0.0.1:0", Unix: filepath.Join(dir, "api.sock"), NATS: "127.0.0.1:-1", Enroll: "127.0.0.1:0"}
	fake := gh.NewFake()
	fake.AddRepo(gh.Repo{Owner: "acme", Name: "app", DefaultBranch: "main", CloneURL: "file:///nowhere"})
	srv, err := controller.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.GH = fake
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Run(ctx) }()
	<-srv.Ready()
	api, _ := client.New("unix://"+cfg.Listen.Unix, nil)
	if _, err := api.CreateTemplate(ctx, []byte("name: tiny\narch: arm64\nmin: {vcpu: 1, ram: 256MiB, disk: 1GiB}\nmax_sessions: 1\nagent:\n  start: [x]\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := api.CreateProject(ctx, proto.ProjectRequest{Slug: "web", Template: ptr("tiny")}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.LinkRepo(ctx, "web", "acme/app", ""); err != nil {
		t.Fatal(err)
	}

	id := 0
	call := func(method string, params any) (json.RawMessage, string) {
		t.Helper()
		id++
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		out, err := api.MCP(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		var resp struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(out, &resp)
		if resp.Error != nil {
			return nil, resp.Error.Message
		}
		return resp.Result, ""
	}
	tool := func(name string, args any) (string, bool) {
		t.Helper()
		res, rpcErr := call("tools/call", map[string]any{"name": name, "arguments": args})
		if rpcErr != "" {
			return rpcErr, true
		}
		var r struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		}
		json.Unmarshal(res, &r)
		if len(r.Content) == 0 {
			t.Fatalf("%s: empty result %s", name, res)
		}
		return strings.NewReplacer("\n", "", ": ", ":", "  ", "").Replace(r.Content[0].Text), r.IsError
	}

	if res, e := call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "t", "version": "1"}}); e != "" || !strings.Contains(string(res), `"wrangl"`) {
		t.Fatalf("initialize: %s %s", res, e)
	}
	res, _ := call("tools/list", map[string]any{})
	for _, want := range []string{`"project_list"`, `"task_queue"`, `"run_list"`, `"project_get"`, `"project_create"`, `"project_edit"`, `"project_archive"`, `"project_restore"`, `"project_delete"`, `"task_create"`, `project slug, for a top-level task`} {
		if !strings.Contains(string(res), want) {
			t.Errorf("tools/list missing %s", want)
		}
	}
	for _, not := range []string{`"ask_question"`, `"submit_review"`, `"propose_split"`, `"task_next"`, `"_system_question"`} {
		if strings.Contains(string(res), not) {
			t.Errorf("tools/list offers %s to the owner", not)
		}
	}

	if out, isErr := tool("project_list", map[string]any{}); isErr || !strings.Contains(out, `"slug":"web"`) || !strings.Contains(out, `"template":"tiny"`) {
		t.Fatalf("project_list: %s", out)
	}
	// project CRUD
	if out, isErr := tool("project_create", map[string]any{"slug": "scratch", "name": "Scratch", "template": "tiny", "priority": 3}); isErr || !strings.Contains(out, `"slug":"scratch"`) || !strings.Contains(out, `"template":"tiny"`) {
		t.Fatalf("project_create: %s", out)
	}
	if out, isErr := tool("project_edit", map[string]any{"slug": "scratch", "description": "temp", "auto_queue": true}); isErr || !strings.Contains(out, `"description":"temp"`) || !strings.Contains(out, `"auto_queue":true`) || !strings.Contains(out, `"name":"Scratch"`) {
		t.Fatalf("project_edit: %s", out)
	}
	if out, isErr := tool("project_get", map[string]any{"slug": "scratch"}); isErr || !strings.Contains(out, `"priority":3`) {
		t.Fatalf("project_get: %s", out)
	}
	if out, isErr := tool("project_archive", map[string]any{"slug": "scratch"}); isErr || !strings.Contains(out, `"archived":true`) {
		t.Fatalf("project_archive: %s", out)
	}
	if out, _ := tool("project_list", map[string]any{"archived": "archived"}); !strings.Contains(out, `"slug":"scratch"`) {
		t.Fatalf("archived list: %s", out)
	}
	if out, isErr := tool("project_restore", map[string]any{"slug": "scratch"}); isErr || !strings.Contains(out, `"archived":false`) {
		t.Fatalf("project_restore: %s", out)
	}
	if out, isErr := tool("project_delete", map[string]any{"slug": "scratch", "confirm": "nope"}); !isErr {
		t.Fatalf("project_delete without confirm succeeded: %s", out)
	}
	if out, isErr := tool("project_delete", map[string]any{"slug": "scratch", "confirm": "scratch"}); isErr || !strings.Contains(out, `"deleted":"scratch"`) {
		t.Fatalf("project_delete: %s", out)
	}
	out, isErr := tool("task_create", map[string]any{"project": "web", "title": "from my laptop agent"})
	var root struct{ Sqid string }
	if json.Unmarshal([]byte(out), &root); isErr || root.Sqid == "" {
		t.Fatalf("task_create: %s", out)
	}
	for _, title := range []string{"first", "second"} {
		if out, isErr := tool("task_create", map[string]any{"parent": root.Sqid, "title": title}); isErr {
			t.Fatalf("subtask: %s", out)
		}
	}
	if out, isErr := tool("task_tree", map[string]any{"sqid": root.Sqid}); isErr || strings.Index(out, "first") > strings.Index(out, "second") {
		t.Fatalf("task_tree: %s", out)
	}
	if out, isErr := tool("task_queue", map[string]any{"sqid": root.Sqid}); isErr || !strings.Contains(out, `"run_id"`) {
		t.Fatalf("task_queue: %s", out)
	}
	if out, isErr := tool("run_list", map[string]any{"project": "web"}); isErr || !strings.Contains(out, "from my laptop agent") {
		t.Fatalf("run_list: %s", out)
	}
	if out, isErr := tool("submit_review", map[string]any{"risk": "low", "summary_md": "x"}); !isErr {
		t.Fatalf("submit_review must be refused: %s %v", out, isErr)
	}
	if out, isErr := tool("task_get", map[string]any{"sqid": "nope"}); !isErr {
		t.Fatalf("unknown task: %s", out)
	}
	if d, _ := api.Task(ctx, root.Sqid); len(d.Children) != 2 {
		t.Fatalf("the MCP writes went through the stack: %+v", d)
	}

	// The stdio bridge, as `claude mcp add wrangl -- wrangl mcp` would run it.
	stdin := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"task_list","arguments":{"text":"laptop"}}}`,
	}, "\n") + "\n"
	cmd := wcli.NewRoot()
	var stdout bytes.Buffer
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--controller", "unix://" + cfg.Listen.Unix, "mcp"})
	bctx, bcancel := context.WithTimeout(ctx, 10*time.Second)
	defer bcancel()
	if err := cmd.ExecuteContext(bctx); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 || !strings.Contains(stdout.String(), "from my laptop agent") || !strings.Contains(stdout.String(), `"id":1`) {
		t.Fatalf("bridge output (want 2 responses, none for the notification):\n%s", stdout.String())
	}
	cancel()
	if err := <-srvDone; err != nil {
		t.Fatal(err)
	}
}

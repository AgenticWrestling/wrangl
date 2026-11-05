package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

const cliToolKDL = `tool "deploy_status" {
  description "Show the deployment status"
  effect "read"
  run-kinds "work" "revise"
  input { param "env" type="string" { enum "staging" "prod" } }
  exec {
    argv "kubectl" "get" "deploy" "-n" "{{input.env}}"
    secret "KUBECONFIG" name="kube.prod" as="file"
  }
}`

type toolCall struct{ Method, Path, Query, Body string }

func toolAPI(t *testing.T, calls *[]toolCall, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*calls = append(*calls, toolCall{r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
		if status != 0 {
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(proto.APIError{Error: "boom"})
			return
		}
		v := proto.LibraryToolView{Name: "deploy_status", Kind: "exec", Effect: "read", Version: 2, RunKinds: []string{"work", "revise"}, Projects: []string{"alpha"}, KDL: cliToolKDL}
		switch {
		case r.Method == "DELETE" || (r.Method == "PUT" && strings.Contains(r.URL.Path, "/projects/")):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "GET" && (r.URL.Path == "/api/v1/tools" || strings.HasSuffix(r.URL.Path, "/projects/alpha/tools")):
			json.NewEncoder(w).Encode([]proto.LibraryToolView{v})
		case strings.HasSuffix(r.URL.Path, "/test"):
			w.WriteHeader(http.StatusNotImplemented)
			json.NewEncoder(w).Encode(proto.APIError{Error: "tool execution is not implemented yet"})
		default:
			json.NewEncoder(w).Encode(v)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func writeTool(t *testing.T, src string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tool.kdl")
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestToolListShow(t *testing.T) {
	var calls []toolCall
	url := toolAPI(t, &calls, 0)
	out, err := run2(t, url, "tool", "list")
	if err != nil || !strings.Contains(out, "deploy_status") || !strings.Contains(out, "work,revise") || !strings.Contains(out, "alpha") {
		t.Fatalf("list: %v %q", err, out)
	}
	if _, err = run2(t, url, "tool", "list", "--project", "alpha"); err != nil || calls[len(calls)-1].Path != "/api/v1/projects/alpha/tools" {
		t.Fatalf("list --project: %v %+v", err, calls)
	}
	out, err = run2(t, url, "tool", "show", "deploy_status")
	if err != nil || !strings.Contains(out, `tool "deploy_status"`) || !strings.Contains(out, "version") {
		t.Fatalf("show: %v %q", err, out)
	}
	if _, err = run2(t, url, "tool", "show"); err == nil {
		t.Fatal("show without a name should fail")
	}
}

func TestToolAddEditRm(t *testing.T) {
	var calls []toolCall
	url := toolAPI(t, &calls, 0)
	f := writeTool(t, cliToolKDL)
	out, err := run2(t, url, "tool", "add", "-f", f)
	last := calls[len(calls)-1]
	if err != nil || !strings.Contains(out, "added tool deploy_status") || last.Method != "POST" || last.Path != "/api/v1/tools" || !strings.Contains(last.Body, "kubectl") {
		t.Fatalf("add: %v %q %+v", err, out, last)
	}
	out, err = run2(t, url, "tool", "edit", "deploy_status", "-f", f)
	last = calls[len(calls)-1]
	if err != nil || !strings.Contains(out, "updated tool deploy_status (v2)") || last.Method != "PUT" || last.Path != "/api/v1/tools/deploy_status" {
		t.Fatalf("edit: %v %q %+v", err, out, last)
	}
	if _, err = run2(t, url, "tool", "rm", "deploy_status", "--force"); err != nil || calls[len(calls)-1].Query != "force=1" {
		t.Fatalf("rm: %v %+v", err, calls[len(calls)-1])
	}
	if _, err = run2(t, url, "tool", "rm", "deploy_status"); err != nil || calls[len(calls)-1].Query != "" {
		t.Fatalf("rm: %v %+v", err, calls[len(calls)-1])
	}
}

func TestToolAddRejectsBadFileBeforeCalling(t *testing.T) {
	var calls []toolCall
	url := toolAPI(t, &calls, 0)
	f := writeTool(t, `tool "x" { description "d"; effect "read"; run-kinds "work"; exec { argv "sh" "-c" "id" } }`)
	_, err := run2(t, url, "tool", "add", "-f", f)
	if err == nil || !strings.Contains(err.Error(), "argv-only") || len(calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, calls)
	}
	if _, err = run2(t, url, "tool", "add"); err == nil {
		t.Fatal("add without -f should fail")
	}
	if _, err = run2(t, url, "tool", "add", "-f", filepath.Join(t.TempDir(), "missing.kdl")); err == nil {
		t.Fatal("missing file should fail")
	}
}

func TestToolEnableDisable(t *testing.T) {
	var calls []toolCall
	url := toolAPI(t, &calls, 0)
	out, err := run2(t, url, "tool", "enable", "deploy_status", "--project", "alpha")
	last := calls[len(calls)-1]
	if err != nil || !strings.Contains(out, "enabled deploy_status on alpha") || last.Method != "PUT" || last.Path != "/api/v1/projects/alpha/tools/deploy_status" {
		t.Fatalf("enable: %v %q %+v", err, out, last)
	}
	out, err = run2(t, url, "tool", "disable", "deploy_status", "--project", "alpha")
	last = calls[len(calls)-1]
	if err != nil || !strings.Contains(out, "disabled") || last.Method != "DELETE" {
		t.Fatalf("disable: %v %q %+v", err, out, last)
	}
	if _, err = run2(t, url, "tool", "enable", "deploy_status"); err == nil {
		t.Fatal("--project is required")
	}
}

func TestToolTest(t *testing.T) {
	var calls []toolCall
	url := toolAPI(t, &calls, 0)
	// local validation never contacts the controller
	out, err := run2(t, url, "tool", "test", "-f", writeTool(t, cliToolKDL))
	if err != nil || !strings.Contains(out, "ok: deploy_status") || !strings.Contains(out, "kube.prod") || len(calls) != 0 {
		t.Fatalf("local: %v %q %v", err, out, calls)
	}
	if _, err = run2(t, url, "tool", "test", "-f", writeTool(t, `tool "x" {`)); err == nil {
		t.Fatal("invalid file should fail")
	}
	// on a project: the sample input reaches the test endpoint; not-implemented is reported
	_, err = run2(t, url, "tool", "test", "deploy_status", "--project", "alpha", "--input", `{"env":"prod"}`)
	last := calls[len(calls)-1]
	if err == nil || !strings.Contains(err.Error(), "not implemented") || last.Path != "/api/v1/projects/alpha/tools/deploy_status/test" || !strings.Contains(last.Body, `"env":"prod"`) {
		t.Fatalf("remote: %v %+v", err, last)
	}
	if _, err = run2(t, url, "tool", "test", "deploy_status", "--project", "alpha", "--input", "{nope"); err == nil {
		t.Fatal("bad --input should fail")
	}
	if _, err = run2(t, url, "tool", "test"); err == nil {
		t.Fatal("nothing to test should fail")
	}
}

func TestToolAPIError(t *testing.T) {
	var calls []toolCall
	url := toolAPI(t, &calls, http.StatusConflict)
	if _, err := run2(t, url, "tool", "enable", "deploy_status", "--project", "alpha"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err=%v", err)
	}
}

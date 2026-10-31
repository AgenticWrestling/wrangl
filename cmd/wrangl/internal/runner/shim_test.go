package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeJSONFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claude.json")
	if out, err := mergeJSONFile(p, `{"projects":{"/w/1":{"hasTrustDialogAccepted":true}}}`); err != nil || !strings.Contains(out, "hasTrustDialogAccepted") {
		t.Fatalf("new file: %q %v", out, err)
	}
	os.WriteFile(p, []byte(`{"numStartups":3,"projects":{"/w/0":{"x":1}}}`), 0o600)
	out, err := mergeJSONFile(p, `{"projects":{"/w/1":{"hasTrustDialogAccepted":true}},"theme":"dark"}`)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal([]byte(out), &m)
	projects := m["projects"].(map[string]any)
	if m["numStartups"] != float64(3) || m["theme"] != "dark" || projects["/w/0"] == nil || projects["/w/1"] == nil {
		t.Fatalf("merge lost or missed keys: %s", out)
	}
	os.WriteFile(p, []byte(`[1,2]`), 0o600)
	if _, err := mergeJSONFile(p, `{"a":1}`); err == nil {
		t.Fatal("merged into a non-object")
	}
}

// TestSendHookContinue checks the Stop-hook output each agent format prints.
func TestSendHookContinue(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	reply := map[string]any{"continue": true, "prompt": "You are working on task abc: next\nline two"}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("continue") == "" {
			w.Write([]byte(`{"ok":true}`))
			return
		}
		json.NewEncoder(w).Encode(reply)
	})}
	go srv.Serve(l)
	defer srv.Close()
	t.Setenv("WRANGL_SOCK", sock)
	ctx := context.Background()

	var out bytes.Buffer
	if err := SendHook(ctx, "Stop", []byte(`{"stop_hook_active":false}`), "claude", &out); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got["decision"] != "block" || got["reason"] != reply["prompt"] {
		t.Fatalf("claude format: %q (%v)", out.String(), err)
	}
	out.Reset()
	SendHook(ctx, "Stop", []byte(`{}`), "text", &out)
	if out.String() != reply["prompt"].(string)+"\n" {
		t.Fatalf("text format: %q", out.String())
	}
	for format, decision := range map[string]string{"codex": "block", "agy": "continue", "gemini": "deny"} {
		out.Reset()
		SendHook(ctx, "Stop", []byte(`{}`), format, &out)
		got = nil
		if json.Unmarshal(out.Bytes(), &got); got["decision"] != decision || got["reason"] != reply["prompt"] {
			t.Fatalf("%s format: %q", format, out.String())
		}
	}
	reply = map[string]any{"continue": false}
	out.Reset()
	SendHook(ctx, "Stop", []byte(`{}`), "claude", &out)
	if out.Len() != 0 {
		t.Fatalf("stop must print nothing (exit 0 lets the agent stop): %q", out.String())
	}
	out.Reset()
	SendHook(ctx, "Stop", []byte(`{}`), "agy", &out)
	if strings.TrimSpace(out.String()) != "{}" {
		t.Fatalf("agy stop: %q", out.String())
	}
	out.Reset()
	SendHook(ctx, "PostToolUse", []byte(`not json`), "", &out)
	if out.Len() != 0 {
		t.Fatalf("plain hook printed %q", out.String())
	}
}

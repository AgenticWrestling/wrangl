package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
)

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
	reply = map[string]any{"continue": false}
	out.Reset()
	SendHook(ctx, "Stop", []byte(`{}`), "claude", &out)
	if out.Len() != 0 {
		t.Fatalf("stop must print nothing (exit 0 lets the agent stop): %q", out.String())
	}
	out.Reset()
	SendHook(ctx, "PostToolUse", []byte(`not json`), "", &out)
	if out.Len() != 0 {
		t.Fatalf("plain hook printed %q", out.String())
	}
}

package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The primary session's terminal over the controller's websocket: the same protocol as a run's
// terminal, watch-only until control is taken, and listed on the host runner's terminals.
func TestPrimarySessionTerminalWebsocket(t *testing.T) {
	base, ctx := primaryEnv(t)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	r, err := http.Get(base + "/api/v1/runners/host-test/terms")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(b), `"id":"primary-alpha"`) || !strings.Contains(string(b), `"kind":"primary"`) || !strings.Contains(string(b), `"title":"alpha"`) {
		t.Fatalf("terms: %d %s", r.StatusCode, b)
	}

	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/api/v1/runners/host-test/term/primary-alpha", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	var out strings.Builder
	var msgs []string
	read := func(until func() bool) {
		t.Helper()
		for !until() {
			rctx, rc := context.WithTimeout(ctx, 5*time.Second)
			typ, data, err := c.Read(rctx)
			rc()
			if err != nil {
				t.Fatalf("read: %v (msgs %v, out %q)", err, msgs, out.String())
			}
			if typ == websocket.MessageBinary {
				out.Write(data)
				continue
			}
			var m struct {
				T string `json:"t"`
			}
			json.Unmarshal(data, &m)
			msgs = append(msgs, m.T)
		}
	}
	read(func() bool { return len(msgs) >= 2 })
	if msgs[0] != "hello" {
		t.Fatalf("first message %v", msgs)
	}
	has := func(s string) bool {
		for _, m := range msgs {
			if m == s {
				return true
			}
		}
		return false
	}
	read(func() bool { return has("live") })

	// Watch-only: keystrokes before control go nowhere.
	c.Write(ctx, websocket.MessageBinary, []byte("blocked-input\n"))
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(out.String(), "blocked-input") {
		t.Fatal("input reached the session before control was taken")
	}
	// A resize without control is ignored too.
	c.Write(ctx, websocket.MessageText, []byte(`{"t":"size","cols":40,"rows":10}`))
	c.Write(ctx, websocket.MessageText, []byte(`{"t":"control","on":true}`))
	read(func() bool { return has("control") })
	c.Write(ctx, websocket.MessageBinary, []byte("allowed-input\n"))
	read(func() bool { return strings.Contains(out.String(), "allowed-input") })
	if strings.Contains(out.String(), "blocked-input") {
		t.Error("blocked input surfaced later")
	}
	c.Write(ctx, websocket.MessageText, []byte(`{"t":"size","cols":71,"rows":19}`))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := http.Get(base + "/api/v1/runners/host-test/terms")
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if strings.Contains(string(b), `"cols":71,"rows":19`) {
			goto sized
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the controlling resize did not reach the session")
sized:
	// Unknown or invalid ids are refused; a primary session cannot be closed from the web.
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/api/v1/runners/host-test/term/primary-nope", 404},
		{"GET", "/api/v1/runners/host-test/term/primary-Bad..", 404},
		{"DELETE", "/api/v1/runners/host-test/term/primary-alpha", 409},
	} {
		req, _ := http.NewRequest(c.method, base+c.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want && !(c.method == "GET" && resp.StatusCode == 502) {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
	}
}

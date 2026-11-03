package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// checkTerm drives the web terminal against a live runner: open a shell, watch it, take
// control, type, resize, and close it.
func checkTerm(t *testing.T, ctx context.Context, hc *http.Client, runner string) {
	t.Helper()
	call := func(method, path string) (int, []byte) {
		req, _ := http.NewRequestWithContext(ctx, method, "http://x"+path, nil)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	code, body := call("POST", "/api/v1/runners/"+runner+"/shell")
	var opened struct{ ID string }
	if code != 200 || json.Unmarshal(body, &opened) != nil || !strings.HasPrefix(opened.ID, "sh-") {
		t.Fatalf("open shell: %d %s", code, body)
	}
	if code, body = call("GET", "/api/v1/runners/"+runner+"/terms"); code != 200 || !bytes.Contains(body, []byte(opened.ID)) {
		t.Fatalf("terms: %d %s", code, body)
	}
	if code, _ = call("GET", "/api/v1/runners/"+runner+"/term/not..valid"); code != 400 {
		t.Fatalf("bad id accepted: %d", code)
	}

	dial := func() *websocket.Conn {
		c, _, err := websocket.Dial(ctx, "ws://x/api/v1/runners/"+runner+"/term/"+opened.ID, &websocket.DialOptions{HTTPClient: hc})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	viewer, driver := dial(), dial()
	defer viewer.CloseNow()
	defer driver.CloseNow()

	// Both see a hello with the PTY size, then all output (binary) is shared.
	var seen strings.Builder
	var texts []string
	readUntil := func(c *websocket.Conn, want string) {
		t.Helper()
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		for !strings.Contains(seen.String(), want) {
			typ, data, err := c.Read(rctx)
			if err != nil {
				t.Fatalf("waiting for %q: %v (saw %q, texts %v)", want, err, seen.String(), texts)
			}
			if typ == websocket.MessageBinary {
				seen.Write(data)
			} else {
				texts = append(texts, string(data))
			}
		}
	}
	send := func(c *websocket.Conn, typ websocket.MessageType, b string) {
		if err := c.Write(ctx, typ, []byte(b)); err != nil {
			t.Fatal(err)
		}
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, hello, err := viewer.Read(rctx)
	cancel()
	if err != nil || !strings.Contains(string(hello), `"t":"hello"`) || !strings.Contains(string(hello), `"cols":120`) {
		t.Fatalf("hello: %s %v", hello, err)
	}
	driver.Read(ctx) // its hello

	// Input without control is dropped; with control it reaches the shell.
	send(viewer, websocket.MessageBinary, "echo nope-$((1+1))\n")
	send(driver, websocket.MessageText, `{"t":"control","on":true}`)
	send(driver, websocket.MessageText, `{"t":"size","cols":100,"rows":30}`)
	send(driver, websocket.MessageBinary, "printf 'wterm-%s\\n' 42; stty size\n")
	readUntil(driver, "wterm-42")
	readUntil(driver, "30 100") // the resize reached the PTY
	if strings.Contains(seen.String(), "nope-2") {
		t.Fatal("a view-only connection typed into the shell")
	}
	if code, body = call("GET", "/api/v1/runners/"+runner+"/terms"); !bytes.Contains(body, []byte(`"cols":100`)) {
		t.Fatalf("terms after resize: %d %s", code, body)
	}

	// Closing the shell ends the stream.
	if code, body = call("DELETE", "/api/v1/runners/"+runner+"/term/"+opened.ID); code != 200 {
		t.Fatalf("close: %d %s", code, body)
	}
	endCtx, endCancel := context.WithTimeout(ctx, 15*time.Second)
	defer endCancel()
	for {
		typ, data, err := driver.Read(endCtx)
		if err != nil {
			t.Fatalf("no end frame: %v", err)
		}
		if typ == websocket.MessageText && strings.Contains(string(data), `"t":"end"`) {
			break
		}
	}
}

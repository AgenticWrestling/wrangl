package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func server() *Server {
	return &Server{Name: "t", Version: "1", Tools: []Tool{{Name: "echo", Description: "echo", InputSchema: Schema(map[string]string{"text": "string"}, "text")}, {Name: "fail", InputSchema: Schema(nil)}},
		Call: func(_ context.Context, name string, args json.RawMessage) (any, error) {
			if name == "fail" {
				return nil, errors.New("nope")
			}
			var a struct{ Text string }
			json.Unmarshal(args, &a)
			return map[string]string{"echo": a.Text}, nil
		}}
}

func TestStdioSession(t *testing.T) {
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fail"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"missing"}}`,
		`{"jsonrpc":"2.0","id":6,"method":"bogus"}`,
	}, "\n")
	var out bytes.Buffer
	if err := server().Serve(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		json.Unmarshal([]byte(line), &m)
		b, _ := json.Marshal(m["id"])
		byID[string(b)] = m
	}
	if len(byID) != 6 {
		t.Fatalf("want 6 responses (notification unanswered), got %d:\n%s", len(byID), out.String())
	}
	if v := byID["1"]["result"].(map[string]any)["protocolVersion"]; v != "2025-03-26" {
		t.Fatalf("negotiated %v", v)
	}
	if tools := byID["2"]["result"].(map[string]any)["tools"].([]any); len(tools) != 2 {
		t.Fatalf("tools %v", tools)
	}
	call := byID["3"]["result"].(map[string]any)
	if call["isError"] != false || !strings.Contains(call["content"].([]any)[0].(map[string]any)["text"].(string), `"echo": "hi"`) {
		t.Fatalf("call %v", call)
	}
	if byID["4"]["result"].(map[string]any)["isError"] != true {
		t.Fatal("tool error not flagged")
	}
	if byID["5"]["error"].(map[string]any)["code"].(float64) != -32602 || byID["6"]["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("errors: %v %v", byID["5"], byID["6"])
	}
}

func TestHTTPTransport(t *testing.T) {
	srv := httptest.NewServer(server().HTTPHandler())
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":"a","method":"ping"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(err, resp.Status)
	}
	resp, _ = http.Post(srv.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification: %s", resp.Status)
	}
	if resp, _ := http.Get(srv.URL); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %s", resp.Status)
	}
}

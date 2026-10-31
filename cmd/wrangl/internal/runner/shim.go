package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/nreid/devhost/cmd/wrangl/internal/mcp"
)

// SocketPath is where the agent finds the daemon: $WRANGL_SOCK, else the default.
func SocketPath() string {
	if p := os.Getenv("WRANGL_SOCK"); p != "" {
		return p
	}
	return DefaultSocketPath
}

func post(ctx context.Context, path, url string, body []byte) ([]byte, int, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://wrangl"+url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := SocketClient(path).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return b, resp.StatusCode, err
}

// SendHook forwards a hook event (stdin JSON) to the daemon. With continueFormat set (a
// Stop hook in interactive drive) it prints the agent-specific continue response.
func SendHook(ctx context.Context, event string, payload []byte, continueFormat string, out io.Writer) error {
	if !json.Valid(payload) {
		payload, _ = json.Marshal(string(payload))
	}
	body, _ := json.Marshal(map[string]any{"event": event, "payload": json.RawMessage(payload)})
	url := "/hook"
	if continueFormat != "" {
		url += "?continue=" + continueFormat
	}
	resp, code, err := post(ctx, SocketPath(), url, body)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("daemon: HTTP %d: %s", code, resp)
	}
	if continueFormat == "" {
		return nil
	}
	var c struct {
		Continue bool   `json:"continue"`
		Prompt   string `json:"prompt"`
	}
	json.Unmarshal(resp, &c)
	if !c.Continue {
		return nil // let the agent stop
	}
	switch continueFormat {
	case "claude": // Claude Code Stop hook decision control
		return json.NewEncoder(out).Encode(map[string]string{"decision": "block", "reason": c.Prompt})
	default:
		_, err := fmt.Fprintln(out, c.Prompt)
		return err
	}
}

// CallTool calls one work-stack tool through the daemon and returns its JSON result.
func CallTool(ctx context.Context, name string, args []byte) ([]byte, error) {
	if len(args) == 0 {
		args = []byte("{}")
	}
	resp, code, err := post(ctx, SocketPath(), "/tool/"+name, args)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(resp, &e)
		if e.Error == "" {
			e.Error = string(resp)
		}
		return nil, fmt.Errorf("%s", e.Error)
	}
	return resp, nil
}

// ServeMCP runs the stdio MCP server the agent spawns: the tool list comes from the
// daemon (it depends on the run's kind) and every call is relayed through it.
func ServeMCP(ctx context.Context, in io.Reader, out io.Writer, version string) error {
	tools := []mcp.Tool{}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://wrangl/tools", nil)
	if resp, err := SocketClient(SocketPath()).Do(req); err == nil {
		json.NewDecoder(resp.Body).Decode(&tools)
		resp.Body.Close()
	}
	srv := &mcp.Server{Name: "wrangl", Version: version, Tools: tools,
		Call: func(ctx context.Context, name string, args json.RawMessage) (any, error) {
			b, err := CallTool(ctx, name, args)
			if err != nil {
				return nil, err
			}
			return string(b), nil
		}}
	return srv.Serve(ctx, in, out)
}

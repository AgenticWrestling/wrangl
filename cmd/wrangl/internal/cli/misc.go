package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/client"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/config"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/tui"
)

func tuiCmd(g *Global) *cobra.Command {
	return &cobra.Command{
		Use: "tui", Short: "Full-screen dashboard: runners, runs, work stack, questions, PRs", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			self, err := os.Executable()
			if err != nil {
				return err
			}
			p, _ := clientConfigPath(g)
			cc, _ := config.LoadClient(p)
			url := config.Resolve(g.Controller, "WRANGL_CONTROLLER", cc.Controller, config.DefaultLocalController)
			execFn := func(args ...string) tea.Cmd {
				full := append([]string{"--controller", url}, args...)
				var c *exec.Cmd
				if args[0] == "run" && args[1] == "attach" || args[0] == "logs" {
					c = exec.Command(self, full...) // live: until the run ends or Ctrl-C
				} else {
					pager := os.Getenv("PAGER")
					if pager == "" {
						pager = "less -R"
					}
					c = exec.Command("/bin/sh", append([]string{"-c", `"$0" "$@" | ` + pager, self}, full...)...)
				}
				return tea.ExecProcess(c, func(err error) tea.Msg { return nil })
			}
			_, err = tea.NewProgram(tui.New(cmd.Context(), c, execFn), tea.WithAltScreen(), tea.WithContext(cmd.Context())).Run()
			if errors.Is(err, tea.ErrProgramKilled) {
				return nil
			}
			return err
		},
	}
}

func pushCmd(g *Global) *cobra.Command {
	return group("push", "Web push notifications",
		&cobra.Command{Use: "test", Short: "Send a test notification to every subscribed browser", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				c, err := apiClient(g)
				if err != nil {
					return err
				}
				n, err := c.PushTest(cmd.Context())
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "delivered to %d browser(s)\n", n)
				return nil
			}},
	)
}

// mcpCmd bridges stdio MCP to the controller's /mcp endpoint, so a local agent can use
// the work-stack tools with the CLI's own controller URL and Cloudflare service token:
//
//	claude mcp add wrangl -- wrangl mcp
func mcpCmd(g *Global) *cobra.Command {
	return &cobra.Command{
		Use: "mcp", Short: "Serve the work-stack tools to a local agent over stdio (relayed to the controller)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			return relayMCP(cmd.Context(), c, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

// relayMCP forwards newline-delimited JSON-RPC messages; a transport failure becomes a
// JSON-RPC error for that request so the agent sees why.
func relayMCP(ctx context.Context, c *client.Client, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var mu sync.Mutex
	var wg sync.WaitGroup
	write := func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		out.Write(append(bytes.TrimSpace(b), '\n'))
	}
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := c.MCP(ctx, line)
			if err != nil {
				var req struct {
					ID json.RawMessage `json:"id"`
				}
				if json.Unmarshal(line, &req) == nil && len(req.ID) > 0 {
					b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID,
						"error": map[string]any{"code": -32603, "message": err.Error()}})
					write(b)
				}
				return
			}
			if resp != nil {
				write(resp)
			}
		}()
	}
	wg.Wait()
	return sc.Err()
}

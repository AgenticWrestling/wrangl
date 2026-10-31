package cli

import (
	"errors"
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/nreid/devhost/cmd/wrangl/internal/config"
	"github.com/nreid/devhost/cmd/wrangl/internal/tui"
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

func pushCmd() *cobra.Command {
	return group("push", "Web push notifications",
		leaf("test", "Send a test notification to every subscription", "M8", cobra.NoArgs),
	)
}

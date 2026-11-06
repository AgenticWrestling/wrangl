package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/agentplugin"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner"
)

// newSetupCmd is the manual half of what `controller serve` and the runner daemon do at
// startup. It exists because the automatic path is deliberately silent about failure -- it must
// never stop a daemon starting -- and a developer who wants the skill now, or who wants to see
// why it did not install, needs a command that REFUSES instead of logging a warning.
func newSetupCmd() *cobra.Command {
	var dir string
	var skipClaude bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Install the wrangl skill into this host's Claude Code",
		Long: `Install the wrangl skill into this host's Claude Code.

The skill is embedded in this binary and versioned with it. setup writes it to
~/.wrangl/agent-plugin, registers that directory with Claude as a plugin marketplace, and
reloads it:

    claude plugin marketplace add ~/.wrangl/agent-plugin --scope user
    claude plugin install wrangl@wrangl
    claude plugin marketplace update wrangl

Nothing is written to ~/.claude. Claude records the install itself, so
` + "`claude plugin disable wrangl`" + ` and ` + "`claude plugin remove wrangl`" + ` are the way out.

` + "`controller serve`" + ` and the runner daemon do this on startup, so an upgrade carries the
skill with it. This command is for installing it on a machine that runs neither, and for
seeing the error when the automatic path only logged a warning.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dir == "" {
				d, err := agentplugin.DefaultDir()
				if err != nil {
					return err
				}
				dir = d
			}
			changed, err := agentplugin.Write(dir, runner.Version)
			if err != nil {
				return fmt.Errorf("write the skill to %s: %w", dir, err)
			}
			out := cmd.OutOrStdout()
			if changed {
				fmt.Fprintf(out, "wrote %s (version %s)\n", dir, runner.Version)
			} else {
				fmt.Fprintf(out, "%s is already at version %s\n", dir, runner.Version)
			}
			if skipClaude {
				fmt.Fprintf(out, "skipped claude: register it yourself with\n  claude plugin marketplace add %s --scope user\n  claude plugin install %s@%s\n",
					dir, agentplugin.PluginName, agentplugin.MarketplaceName)
				return nil
			}
			// Unlike Ensure, these refuse. That is the whole reason this command exists.
			if err := agentplugin.Register(cmd.Context(), dir); err != nil {
				return fmt.Errorf("register with claude: %w", err)
			}
			if err := agentplugin.Reload(cmd.Context()); err != nil {
				return fmt.Errorf("reload the marketplace: %w", err)
			}
			fmt.Fprintf(out, "installed %s@%s and reloaded it\n", agentplugin.PluginName, agentplugin.MarketplaceName)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "where to write the plugin (default: ~/.wrangl/agent-plugin)")
	cmd.Flags().BoolVar(&skipClaude, "no-claude", false, "write the files but do not invoke claude")
	return cmd
}

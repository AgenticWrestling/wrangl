package cli

import "github.com/spf13/cobra"

func runnerCmd(g *Global) *cobra.Command {
	bootstrap := leaf("bootstrap", "Enrol this instance (run once by cloud-init)", "M2", cobra.NoArgs)
	bf := bootstrap.Flags()
	bf.String("token", "", "one-time enrolment token (required)")
	bf.String("sha256", "", "expected sha256 of this binary")
	bf.String("name", "", "runner name (default: assigned by the controller)")

	hook := leaf("hook <event>", "Agent hook entrypoint: forwards stdin JSON to the daemon", "M5", cobra.ExactArgs(1))
	hook.Hidden = true
	hook.Flags().String("continue-format", "", "print an agent-specific continue response (interactive drive)")
	mcp := leaf("mcp", "stdio MCP server for the agent", "M5", cobra.NoArgs)
	mcp.Hidden = true

	return group("runner", "Runner mode (runs on a runner instance)",
		bootstrap,
		leaf("run", "Runner daemon (systemd ExecStart)", "M2", cobra.NoArgs),
		hook, mcp,
		leaf("status", "Local runner status", "M2", cobra.NoArgs),
	)
}

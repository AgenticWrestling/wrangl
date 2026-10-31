package cli

import "github.com/spf13/cobra"

func tuiCmd() *cobra.Command {
	return leaf("tui", "Full-screen dashboard", "M7", cobra.NoArgs)
}

func pushCmd() *cobra.Command {
	return group("push", "Web push notifications",
		leaf("test", "Send a test notification to every subscription", "M8", cobra.NoArgs),
	)
}

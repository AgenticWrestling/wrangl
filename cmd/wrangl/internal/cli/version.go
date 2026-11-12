package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner"
)

// versionCmd prints the build's version (a release tag, or a git describe for dev builds).
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the wrangl version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "wrangl %s (%s/%s)\n", runner.Version, runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}

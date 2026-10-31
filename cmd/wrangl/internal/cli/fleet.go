package cli

import (
	"time"

	"github.com/spf13/cobra"
)

func fleetCmd() *cobra.Command {
	launch := leaf("launch", "Launch runners", "M3", cobra.NoArgs)
	launch.Flags().String("template", "", "runner template (required)")
	launch.Flags().Int("count", 1, "how many")
	token := leaf("enroll-token", "Mint a one-time enrolment token (adopt an existing box)", "M2", cobra.NoArgs)
	token.Flags().String("template", "", "runner template (required)")
	token.Flags().Duration("ttl", 30*time.Minute, "token lifetime")
	return group("fleet", "Manage runners",
		leaf("list", "List runners", "M2", cobra.NoArgs),
		leaf("show <runner>", "Show a runner", "M2", cobra.ExactArgs(1)),
		launch,
		leaf("cordon <runner>", "Stop assigning new work", "M3", cobra.ExactArgs(1)),
		leaf("uncordon <runner>", "Resume assigning work", "M3", cobra.ExactArgs(1)),
		leaf("drain <runner>", "Finish current work, then terminate", "M3", cobra.ExactArgs(1)),
		leaf("terminate <runner>", "Terminate now", "M3", cobra.ExactArgs(1)),
		token,
	)
}

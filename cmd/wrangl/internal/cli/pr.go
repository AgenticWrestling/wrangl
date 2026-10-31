package cli

import "github.com/spf13/cobra"

func prCmd() *cobra.Command {
	list := leaf("list", "List pull requests", "M6", cobra.NoArgs)
	list.Flags().String("state", "open", "open|merged|closed|all")
	rc := leaf("request-changes <pr>", "Request changes (queues a revise run)", "M6", cobra.ExactArgs(1))
	rc.Flags().StringP("message", "m", "", "what to change (required)")
	return group("pr", "Review and merge pull requests",
		list,
		leaf("show <pr>", "Show a PR with its review and gates", "M6", cobra.ExactArgs(1)),
		leaf("approve <pr>", "Approve and merge", "M6", cobra.ExactArgs(1)),
		rc,
		leaf("close <pr>", "Close without merging", "M6", cobra.ExactArgs(1)),
	)
}

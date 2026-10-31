package cli

import "github.com/spf13/cobra"

func runCmd() *cobra.Command {
	list := leaf("list", "List runs", "M4", cobra.NoArgs)
	list.Flags().String("state", "", "only this state")
	list.Flags().String("runner", "", "only this runner")
	list.Flags().String("project", "", "only this project")
	diff := leaf("diff <run>", "Show the run's diff", "M4", cobra.ExactArgs(1))
	diff.Flags().Bool("stat", false, "summary only")
	return group("run", "Inspect and control runs",
		list,
		leaf("show <run>", "Show a run", "M4", cobra.ExactArgs(1)),
		leaf("cancel <run>", "Cancel a run", "M4", cobra.ExactArgs(1)),
		leaf("attach <run>", "Follow the run's terminal (read-only)", "M4", cobra.ExactArgs(1)),
		diff,
		leaf("files <run> [path]", "List files in the run's workspace", "M4", cobra.RangeArgs(1, 2)),
		leaf("cat <run> <path>", "Print a file from the run's workspace", "M4", cobra.ExactArgs(2)),
	)
}

func logsCmd() *cobra.Command {
	logs := leaf("logs <run>", "Show a run's logs", "M4", cobra.ExactArgs(1))
	f := logs.Flags()
	f.BoolP("follow", "f", false, "follow new lines")
	f.String("grep", "", "only lines matching this regexp")
	f.String("stream", "", "agent|runner|hook|system")
	f.String("since", "", "only lines newer than this duration or RFC 3339 time")

	search := leaf("search <query>", "Full-text search across logs", "M4", cobra.ExactArgs(1))
	sf := search.Flags()
	sf.String("project", "", "only this project")
	sf.String("runner", "", "only this runner")
	sf.String("since", "", "only lines newer than this duration or RFC 3339 time")
	sf.Int("limit", 100, "maximum results")
	logs.AddCommand(search)
	return logs
}

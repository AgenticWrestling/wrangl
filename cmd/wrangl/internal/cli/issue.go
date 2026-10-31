package cli

import "github.com/spf13/cobra"

func issueCmd() *cobra.Command {
	list := leaf("list", "List synced GitHub issues", "M4", cobra.NoArgs)
	list.Flags().String("project", "", "only this project")
	list.Flags().String("state", "open", "open|closed|all")
	toTask := leaf("to-task <issue>", "Create a task from an issue", "M4", cobra.ExactArgs(1))
	toTask.Flags().String("parent", "", "parent task sqid")
	toTask.Flags().String("template", "", "runner template")
	return group("issue", "GitHub issues",
		leaf("sync <project>", "Sync issues from the project's repos", "M4", cobra.ExactArgs(1)),
		list,
		leaf("show <issue>", "Show an issue", "M4", cobra.ExactArgs(1)),
		toTask,
	)
}

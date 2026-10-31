package cli

import "github.com/spf13/cobra"

func repoCmd() *cobra.Command {
	link := leaf("link <project> <owner/name>", "Link a GitHub repo to a project", "M4", cobra.ExactArgs(2))
	link.Flags().String("branch", "", "base branch (default: the repo's default branch)")
	list := leaf("list", "List linked repos", "M4", cobra.NoArgs)
	list.Flags().String("project", "", "only this project")
	return group("repo", "Link GitHub repos to projects",
		link,
		leaf("unlink <project> <owner/name>", "Unlink a repo", "M4", cobra.ExactArgs(2)),
		list,
	)
}

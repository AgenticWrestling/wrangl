package cli

import "github.com/spf13/cobra"

func taskCmd() *cobra.Command {
	create := leaf("create <project>", "Create a task", "M4", cobra.ExactArgs(1))
	cf := create.Flags()
	cf.String("title", "", "title (required)")
	cf.String("prompt", "", "prompt text")
	cf.StringP("file", "f", "", "read the prompt from a markdown file")
	cf.String("parent", "", "parent task sqid (default: top level)")
	cf.String("repo", "", "owner/name (default: inherited)")
	cf.String("template", "", "runner template (default: inherited)")
	cf.String("issue", "", "linked GitHub issue")
	cf.StringSlice("needs", nil, "precursor task sqids")
	positionFlags(create)
	create.MarkFlagsMutuallyExclusive("prompt", "file")

	tree := leaf("tree [project|sqid]", "Show the ranked task tree", "M4", cobra.MaximumNArgs(1))
	tree.Flags().Bool("all", false, "include done and cancelled tasks")

	list := leaf("list", "List tasks", "M4", cobra.NoArgs)
	list.Flags().String("project", "", "only this project")
	list.Flags().String("state", "", "only this state")
	list.Flags().String("parent", "", "only children of this sqid")

	edit := leaf("edit <sqid>", "Edit a task's title or prompt", "M4", cobra.ExactArgs(1))
	edit.Flags().String("title", "", "new title")
	edit.Flags().String("prompt", "", "new prompt")
	edit.Flags().StringP("file", "f", "", "read the new prompt from a file")
	edit.MarkFlagsMutuallyExclusive("prompt", "file")

	move := leaf("move <sqid>", "Move or rerank a task", "M4", cobra.ExactArgs(1))
	move.Flags().String("parent", "", "new parent sqid")
	move.Flags().Bool("root", false, "move to top level")
	move.MarkFlagsMutuallyExclusive("parent", "root")
	positionFlags(move)

	split := leaf("split <sqid>", "Split a task into subtasks", "M4", cobra.ExactArgs(1))
	sf := split.Flags()
	sf.StringP("file", "f", "", "outline file: one subtask per line, indent to nest")
	sf.StringArray("title", nil, "subtask title (repeatable)")
	sf.Bool("chain", false, "each subtask needs the previous one")
	sf.Bool("copy-prompt", false, "copy the parent prompt into each subtask")
	sf.Bool("agent", false, "queue a plan run to propose the split (milestone M8)")
	sf.String("guidance", "", "guidance for the planning agent (with --agent)")
	split.MarkFlagsMutuallyExclusive("file", "title", "agent")

	reason := func(c *cobra.Command) *cobra.Command {
		c.Flags().StringP("message", "m", "", "reason or summary")
		return c
	}
	return group("task", "Manage the hierarchical work stack",
		create, tree, list,
		leaf("show <sqid>", "Show a task", "M4", cobra.ExactArgs(1)),
		edit, move,
		group("needs", "Manage precursor tasks",
			leaf("add <sqid> <precursor-sqid>", "Add a precursor", "M4", cobra.ExactArgs(2)),
			leaf("rm <sqid> <precursor-sqid>", "Remove a precursor", "M4", cobra.ExactArgs(2)),
		),
		split,
		leaf("queue <sqid>", "Queue a work run for this subtree", "M4", cobra.ExactArgs(1)),
		reason(leaf("complete <sqid>", "Mark a task complete", "M4", cobra.ExactArgs(1))),
		leaf("reopen <sqid>", "Reopen a task", "M4", cobra.ExactArgs(1)),
		reason(leaf("cancel <sqid>", "Cancel a task", "M4", cobra.ExactArgs(1))),
		leaf("delete <sqid>", "Delete a task and its subtree", "M4", cobra.ExactArgs(1)),
	)
}

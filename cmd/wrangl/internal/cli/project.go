package cli

import "github.com/spf13/cobra"

func projectFlags(c *cobra.Command) {
	f := c.Flags()
	f.String("name", "", "display name")
	f.String("description", "", "description")
	f.String("template", "", "default runner template")
	f.String("plan-template", "", "runner template for plan runs (default: --template)")
	f.Bool("auto-merge", false, "auto-merge PRs that pass every gate and review as low risk")
	f.Bool("auto-queue", false, "queue top-level tasks in rank order automatically")
	f.Int("priority", 0, "scheduling priority across projects (higher first)")
}

func projectCmd() *cobra.Command {
	create := leaf("create <slug>", "Create a project", "M4", cobra.ExactArgs(1))
	projectFlags(create)
	edit := leaf("edit <slug>", "Edit a project", "M4", cobra.ExactArgs(1))
	projectFlags(edit)
	return group("project", "Manage projects",
		create,
		leaf("list", "List projects", "M4", cobra.NoArgs),
		leaf("show <slug>", "Show a project", "M4", cobra.ExactArgs(1)),
		edit,
		leaf("delete <slug>", "Delete a project and its tasks", "M4", cobra.ExactArgs(1)),
	)
}

package cli

import "github.com/spf13/cobra"

func templateCmd() *cobra.Command {
	create := leaf("create", "Create a runner template from YAML", "M3", cobra.NoArgs)
	create.Flags().StringP("file", "f", "", "template YAML (required)")
	edit := leaf("edit <name>", "Replace a template (bumps its version)", "M3", cobra.ExactArgs(1))
	edit.Flags().StringP("file", "f", "", "template YAML (default: open $EDITOR)")
	return group("template", "Runner templates (mise config, specs, agent)",
		create,
		leaf("list", "List templates", "M3", cobra.NoArgs),
		leaf("show <name>", "Show a template", "M3", cobra.ExactArgs(1)),
		edit,
		leaf("delete <name>", "Delete a template", "M3", cobra.ExactArgs(1)),
	)
}

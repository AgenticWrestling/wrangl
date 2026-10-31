package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

func repoCmd(g *Global) *cobra.Command {
	var branch, project string
	link := &cobra.Command{
		Use: "link <project> <owner/name>", Short: "Link a GitHub repo (the author App must be installed on it)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			r, err := c.LinkRepo(cmd.Context(), args[0], args[1], branch)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "linked %s to %s (base branch %s)\n", r.Repo, r.Project, r.DefaultBranch)
			return nil
		},
	}
	link.Flags().StringVar(&branch, "branch", "", "base branch (default: the repo's default branch)")
	unlink := &cobra.Command{
		Use: "unlink <project> <owner/name>", Short: "Unlink a repo", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			return c.UnlinkRepo(cmd.Context(), args[0], args[1])
		},
	}
	list := &cobra.Command{
		Use: "list", Short: "List linked repos", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			rs, err := c.Repos(cmd.Context(), project)
			if err != nil {
				return err
			}
			return emit(cmd, g, rs, func(w io.Writer) {
				fmt.Fprintln(w, "PROJECT\tREPO\tBRANCH")
				for _, r := range rs {
					fmt.Fprintf(w, "%s\t%s\t%s\n", r.Project, r.Repo, r.DefaultBranch)
				}
			})
		},
	}
	list.Flags().StringVar(&project, "project", "", "only this project")
	return group("repo", "Link GitHub repos to projects", link, unlink, list)
}

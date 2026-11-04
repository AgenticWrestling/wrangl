package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

func issueCmd(g *Global) *cobra.Command {
	var project, state, parent, template string
	sync := &cobra.Command{
		Use: "sync <project>", Short: "Sync issues from the project's repos", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			n, err := c.SyncIssues(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "synced %d issues\n", n)
			return nil
		},
	}
	var repair bool
	drift := &cobra.Command{
		Use: "drift <project>", Short: "List issues whose synced copy differs from GitHub (--repair re-syncs them)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			ds, err := c.IssueDrift(cmd.Context(), args[0], repair)
			if err != nil {
				return err
			}
			return emit(cmd, g, ds, func(w io.Writer) {
				if len(ds) == 0 {
					fmt.Fprintln(w, "no drift")
					return
				}
				fmt.Fprintln(w, "ISSUE\tDRIFT\tFIELDS")
				for _, d := range ds {
					fmt.Fprintf(w, "%s\t%s\t%v\n", d.Ref, d.Kind, d.Fields)
				}
			})
		},
	}
	drift.Flags().BoolVar(&repair, "repair", false, "re-sync differing rows and drop rows GitHub no longer has")
	list := &cobra.Command{
		Use: "list", Short: "List synced issues", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			is, err := c.Issues(cmd.Context(), project, state)
			if err != nil {
				return err
			}
			return emit(cmd, g, is, func(w io.Writer) {
				fmt.Fprintln(w, "ISSUE\tSTATE\tTASK\tTITLE")
				for _, i := range is {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", i.Ref, i.State, dash(i.Task), i.Title)
				}
			})
		},
	}
	list.Flags().StringVar(&project, "project", "", "only this project")
	list.Flags().StringVar(&state, "state", "open", "open|closed|all")
	needProject := func(c *cobra.Command) {
		c.Flags().StringVar(&project, "project", "", "project the issue's repo is linked to (required)")
		c.MarkFlagRequired("project")
	}
	show := &cobra.Command{
		Use: "show <owner/name#number>", Short: "Show an issue", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			i, err := c.Issue(cmd.Context(), project, args[0])
			if err != nil {
				return err
			}
			return emit(cmd, g, i, func(w io.Writer) {
				fmt.Fprintf(w, "issue\t%s\ntitle\t%s\nstate\t%s\nurl\t%s\n\n%s\n", i.Ref, i.Title, i.State, i.URL, i.Body)
			})
		},
	}
	needProject(show)
	toTask := &cobra.Command{
		Use: "to-task <owner/name#number>", Short: "Create a task from an issue", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			t, err := c.IssueToTask(cmd.Context(), project, args[0], parent, template)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created task %s: %s\n", t.Sqid, t.Title)
			return nil
		},
	}
	needProject(toTask)
	toTask.Flags().StringVar(&parent, "parent", "", "parent task sqid")
	toTask.Flags().StringVar(&template, "template", "", "runner template")
	return group("issue", "GitHub issues", sync, drift, list, show, toTask)
}

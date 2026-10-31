package cli

import (
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"
)

func prCmd(g *Global) *cobra.Command {
	var state string
	list := &cobra.Command{
		Use: "list", Short: "List pull requests", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			ps, err := c.PRs(cmd.Context(), state)
			if err != nil {
				return err
			}
			return emit(cmd, g, ps, func(w io.Writer) {
				fmt.Fprintln(w, "ID\tPR\tSTATE\tCHECKS\tRISK\tDECISION\tTASK")
				for _, p := range ps {
					fmt.Fprintf(w, "%d\t%s#%d\t%s\t%s\t%s\t%s\t%s %s\n", p.ID, p.Repo, p.Number, p.State, dash(p.Checks), dash(p.Risk), dash(p.Decision), p.Task, p.Title)
				}
			})
		},
	}
	list.Flags().StringVar(&state, "state", "open", "open|merged|closed|all")
	id := func(s string) (int64, error) {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("PR id %q: want the wrangl id from `wrangl pr list`", s)
		}
		return v, nil
	}
	show := &cobra.Command{
		Use: "show <pr>", Short: "Show a PR with its review and gates", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := id(args[0])
			if err != nil {
				return err
			}
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			p, err := c.PR(cmd.Context(), n)
			if err != nil {
				return err
			}
			return emit(cmd, g, p, func(w io.Writer) {
				fmt.Fprintf(w, "pr\t%s#%d %s\nurl\t%s\ntask\t%s %s\nbranch\t%s\nstate\t%s\nchecks\t%s\nrisk\t%s\ndecision\t%s\ngates\t%s\n",
					p.Repo, p.Number, p.State, p.URL, p.Task, p.Title, p.Branch, p.State, dash(p.Checks), dash(p.Risk), dash(p.Decision), string(p.Gates))
				if p.Review != "" {
					fmt.Fprintf(w, "\n%s\n", p.Review)
				}
			})
		},
	}
	action := func(op, short string, needMsg bool) *cobra.Command {
		var msg string
		c := &cobra.Command{Use: op + " <pr>", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				n, err := id(args[0])
				if err != nil {
					return err
				}
				cl, err := apiClient(g)
				if err != nil {
					return err
				}
				if err := cl.PRAction(cmd.Context(), n, op, msg); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "PR %d: %s\n", n, op)
				return nil
			}}
		if needMsg {
			c.Flags().StringVarP(&msg, "message", "m", "", "what to change (required)")
			c.MarkFlagRequired("message")
		}
		return c
	}
	return group("pr", "Review and merge pull requests",
		list, show,
		action("approve", "Approve and merge (reviewer App)", false),
		action("request-changes", "Request changes (queues a revise run on the branch)", true),
		action("close", "Close without merging (its tasks reopen)", false),
	)
}

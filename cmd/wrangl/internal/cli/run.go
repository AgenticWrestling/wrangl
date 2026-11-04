package cli

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func runArg(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("run id %q: want a number", s)
	}
	return id, nil
}

func runCmd(g *Global) *cobra.Command {
	var state, runnerName, project string
	var limit int
	list := &cobra.Command{
		Use: "list", Short: "List runs (newest first)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			rs, err := c.Runs(cmd.Context(), state, runnerName, project, limit)
			if err != nil {
				return err
			}
			return emit(cmd, g, rs, func(w io.Writer) {
				fmt.Fprintln(w, "RUN\tSTATE\tPROJECT\tTASK\tRUNNER\tCURRENT\tSTATUS")
				for _, r := range rs {
					fmt.Fprintf(w, "%d\t%s\t%s\t%s %s\t%s\t%s\t%s\n", r.ID, r.State, r.Project, r.Task, r.TaskTitle, dash(r.Runner), dash(r.Current), r.Status)
				}
			})
		},
	}
	list.Flags().StringVar(&state, "state", "", "a state, or 'active'")
	list.Flags().StringVar(&runnerName, "runner", "", "only this runner")
	list.Flags().StringVar(&project, "project", "", "only this project")
	list.Flags().IntVar(&limit, "limit", 30, "maximum runs")

	show := &cobra.Command{
		Use: "show <run>", Short: "Show a run", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := runArg(args[0])
			if err != nil {
				return err
			}
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			r, err := c.Run(cmd.Context(), id)
			if err != nil {
				return err
			}
			return emit(cmd, g, r, func(w io.Writer) {
				fmt.Fprintf(w, "run\t%d (%s)\nstate\t%s\ntask\t%s %s\nproject\t%s\nrunner\t%s\nsession\t%s\nbranch\t%s (pushed: %v)\n",
					r.ID, r.Kind, r.State, r.Task, r.TaskTitle, r.Project, dash(r.Runner), dash(r.Session), r.Branch, r.Pushed)
				fmt.Fprintf(w, "current\t%s\nstatus\t%s\nbase / head\t%s / %s\nqueued\t%s\nstarted\t%s\nfinished\t%s\n",
					dash(r.Current), dash(r.Status), dash(r.BaseSHA), dash(r.HeadSHA), r.QueuedAt, dash(r.StartedAt), dash(r.FinishedAt))
				if r.Error != "" {
					fmt.Fprintf(w, "error\t%s\n", r.Error)
				}
			})
		},
	}
	cancel := &cobra.Command{
		Use: "cancel <run>", Short: "Cancel a run", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := runArg(args[0])
			if err != nil {
				return err
			}
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			if err := c.CancelRun(cmd.Context(), id); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "cancelling run %d\n", id)
			return nil
		},
	}
	attach := &cobra.Command{
		Use: "attach <run>", Short: "Follow the run's terminal (read-only; replays from the start)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := runArg(args[0])
			if err != nil {
				return err
			}
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			return c.AttachPTY(ctx, id, cmd.OutOrStdout())
		},
	}
	inspect := func(use, short, what string, nargs cobra.PositionalArgs, stat bool) *cobra.Command {
		c := &cobra.Command{Use: use, Short: short, Args: nargs,
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := runArg(args[0])
				if err != nil {
					return err
				}
				cl, err := apiClient(g)
				if err != nil {
					return err
				}
				path := ""
				if len(args) > 1 {
					path = args[1]
				}
				st, _ := cmd.Flags().GetBool("stat")
				out, err := cl.Inspect(cmd.Context(), id, what, path, st)
				if err != nil {
					return err
				}
				fmt.Fprint(cmd.OutOrStdout(), out.Data)
				if out.Truncated {
					fmt.Fprintln(cmd.ErrOrStderr(), "\n(truncated at 4 MiB)")
				}
				return nil
			}}
		if stat {
			c.Flags().Bool("stat", false, "summary only")
		}
		return c
	}
	return group("run", "Inspect and control runs",
		list, show, cancel, attach,
		inspect("diff <run>", "Show the run's diff against its base (live from the runner)", "diff", cobra.ExactArgs(1), true),
		inspect("files <run> [path]", "List files in the run's workspace", "files", cobra.RangeArgs(1, 2), false),
		inspect("cat <run> <path>", "Print a file from the run's workspace", "file", cobra.ExactArgs(2), false),
	)
}

func logsCmd(g *Global) *cobra.Command {
	var follow bool
	var grep, stream, since string
	logs := &cobra.Command{
		Use: "logs <run>", Short: "Show a run's logs", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := runArg(args[0])
			if err != nil {
				return err
			}
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			out := cmd.OutOrStdout()
			return c.Logs(ctx, id, follow, grep, stream, since, func(l proto.LogView) {
				if g.Output == "json" {
					emit(cmd, g, l, nil)
					return
				}
				fmt.Fprintf(out, "%s %-6s %s\n", l.TS, l.Stream, l.Message)
			})
		},
	}
	f := logs.Flags()
	f.BoolVarP(&follow, "follow", "f", false, "follow new lines until the run ends")
	f.StringVar(&grep, "grep", "", "only lines matching this regexp")
	f.StringVar(&stream, "stream", "", "agent|runner")
	f.StringVar(&since, "since", "", "only lines newer than a duration (15m) or RFC 3339 time")

	var sProject, sRunner, sSince string
	var sLimit int
	search := &cobra.Command{
		Use: "search <query>", Short: "Full-text search across all run logs (SQLite FTS5 syntax)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			hits, err := c.SearchLogs(cmd.Context(), args[0], sProject, sRunner, sSince, sLimit)
			if err != nil {
				return err
			}
			return emit(cmd, g, hits, func(w io.Writer) {
				fmt.Fprintln(w, "RUN\tTASK\tRUNNER\tTIME\tLINE")
				for _, h := range hits {
					fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", h.RunID, h.Task, dash(h.Runner), h.TS, h.Message)
				}
			})
		},
	}
	sf := search.Flags()
	sf.StringVar(&sProject, "project", "", "only this project")
	sf.StringVar(&sRunner, "runner", "", "only this runner")
	sf.StringVar(&sSince, "since", "", "only lines newer than a duration or RFC 3339 time")
	sf.IntVar(&sLimit, "limit", 100, "maximum results")
	logs.AddCommand(search)
	return logs
}

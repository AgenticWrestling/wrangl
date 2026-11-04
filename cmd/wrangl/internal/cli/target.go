package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

func targetCmd(g *Global) *cobra.Command {
	var all bool
	list := &cobra.Command{
		Use: "list", Short: "List remote targets (machines driven over SSH/ADB; they run no wrangl binary)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			ts, err := c.Targets(cmd.Context(), all)
			if err != nil {
				return err
			}
			return emit(cmd, g, ts, func(w io.Writer) {
				fmt.Fprintln(w, "NAME\tTEMPLATE\tSTATE\tSESSIONS\tPROJECT\tNOTE")
				for _, t := range ts {
					fmt.Fprintf(w, "%s\t%s\t%s\t%d/%d\t%s\t%s\n", t.Name, t.Template, t.State, t.Sessions, t.MaxSessions, dash(t.Project), t.LastError)
				}
			})
		},
	}
	list.Flags().BoolVar(&all, "all", false, "include terminated targets")
	term := &cobra.Command{
		Use: "terminate <name>", Short: "Terminate a target now; runs using it fail as target lost", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			return c.TerminateTarget(cmd.Context(), args[0])
		},
	}
	return group("target", "Remote targets (Mac, Android emulator hosts) driven over SSH/ADB", list, term)
}

package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/policy"
)

func policyCmd(g *Global) *cobra.Command {
	var o policy.Options
	var apply, force bool
	flags := func(c *cobra.Command) {
		f := c.Flags()
		f.StringVar(&o.ManagedPath, "managed-path", "", "managed settings file (default "+policy.DefaultManagedPath+")")
		f.BoolVar(&o.NoManaged, "no-managed", false, "leave the host's managed settings alone")
		f.StringArrayVar(&o.Worktrees, "worktree", nil, "primary worktree to write .claude/settings.json into (absolute path, repeatable)")
		f.StringVar(&o.ScratchBase, "scratch", "", "base directory for session scratch dirs (default <worktree parent>/scratch)")
		f.StringVar(&o.Wrangl, "wrangl-bin", "", "wrangl binary the agent-guard hook runs (default: the one running now)")
	}
	exe := func() {
		if o.Wrangl == "" {
			if p, err := os.Executable(); err == nil {
				o.Wrangl = p
			}
		}
	}

	install := &cobra.Command{
		Use:   "install",
		Short: "Write the session policy files (dry run unless --apply)",
		Long: "Writes the host's managed settings (" + policy.DefaultManagedPath + ", root only) and each --worktree's\n" +
			".claude/settings.json from the defaults built into this binary (ADR 0011 section 6; the lists are proposed).\n" +
			"Without --apply it only prints what it would do. It never replaces an existing file that differs unless --force.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe()
			plan, err := policy.Plan(o)
			if err != nil {
				return err
			}
			res, err := policy.Installer{Apply: apply, Force: force}.Install(plan)
			w := cmd.OutOrStdout()
			for _, r := range res {
				fmt.Fprintf(w, "%s\t%s: %s\n", r.Kind, r.Path, r.Status)
			}
			if err != nil {
				return err
			}
			if !apply {
				fmt.Fprintln(w, "dry run: nothing was written. Add --apply to write the files above.")
			}
			return nil
		},
	}
	flags(install)
	install.Flags().BoolVar(&apply, "apply", false, "write the files (default: dry run)")
	install.Flags().BoolVar(&force, "force", false, "replace an existing file that differs")

	var defaults string
	show := &cobra.Command{
		Use:   "show",
		Short: "Print the policy in force (managed settings and each --worktree) and whether it matches the defaults",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			if defaults != "" { // the embedded default itself
				exe()
				v := policy.Vars{Worktree: "<worktree>", Scratch: "<scratch>", Wrangl: o.Wrangl}
				b, err := policy.Render(defaults, v)
				if err != nil {
					return err
				}
				_, err = w.Write(b)
				return err
			}
			exe()
			shown, err := policy.Show(o)
			if err != nil {
				return err
			}
			return printShown(w, shown)
		},
	}
	flags(show)
	show.Flags().StringVar(&defaults, "defaults", "", "print the built-in default instead: managed, primary or run")

	return group("policy", "Session policy: permission files for interactive and host agents (ADR 0011 section 6)", install, show)
}

func printShown(w io.Writer, shown []policy.Shown) error {
	for _, s := range shown {
		switch {
		case !s.Present:
			fmt.Fprintf(w, "== %s %s: not present\n", s.Kind, s.Path)
		case s.Matches:
			fmt.Fprintf(w, "== %s %s: in force, identical to the defaults\n%s", s.Kind, s.Path, s.Content)
		default:
			fmt.Fprintf(w, "== %s %s: in force, differs from the defaults\n%s", s.Kind, s.Path, s.Content)
		}
	}
	return nil
}

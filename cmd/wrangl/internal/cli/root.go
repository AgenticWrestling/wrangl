// Package cli builds the wrangl cobra command tree. Each noun lives in its own file;
// commands not yet built return ErrNotImplemented naming the milestone that delivers them.
package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// ErrNotImplemented is wrapped by every stub command.
var ErrNotImplemented = errors.New("not implemented yet")

// Global holds the persistent flags shared by every command.
type Global struct {
	Config     string
	Controller string
	Output     string
}

var validOutputs = map[string]bool{"table": true, "json": true, "yaml": true}

// NewRoot builds the whole command tree.
func NewRoot() *cobra.Command {
	g := &Global{}
	root := &cobra.Command{
		Use:           "wrangl",
		Short:         "Controller, runner and client for a fleet of coding-agent runners",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if !validOutputs[g.Output] {
				return fmt.Errorf("invalid --output %q: want table, json or yaml", g.Output)
			}
			return nil
		},
	}
	pf := root.PersistentFlags()
	pf.StringVar(&g.Config, "config", "", "config file (env WRANGL_CONFIG)")
	pf.StringVar(&g.Controller, "controller", "", "controller URL or unix:///path (env WRANGL_CONTROLLER)")
	pf.StringVarP(&g.Output, "output", "o", "table", "output format: table|json|yaml")

	root.AddCommand(
		controllerCmd(g), runnerCmd(g), tuiCmd(),
		projectCmd(), repoCmd(), issueCmd(), taskCmd(), runCmd(), logsCmd(),
		prCmd(), questionCmd(), templateCmd(), fleetCmd(), pushCmd(), configCmd(g),
	)
	return root
}

// stub returns a RunE that reports which milestone delivers the command.
func stub(milestone string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		return fmt.Errorf("%s: %w (milestone %s)", cmd.CommandPath(), ErrNotImplemented, milestone)
	}
}

// group builds a non-runnable parent command.
func group(use, short string, children ...*cobra.Command) *cobra.Command {
	c := &cobra.Command{Use: use, Short: short}
	c.AddCommand(children...)
	return c
}

// leaf builds a stub command with positional-arg validation.
func leaf(use, short, milestone string, args cobra.PositionalArgs) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, Args: args, RunE: stub(milestone)}
}

// positionFlags declares the mutually exclusive sibling-position flags used by
// `task create` and `task move`.
func positionFlags(c *cobra.Command) {
	f := c.Flags()
	f.String("after", "", "place after this sibling sqid")
	f.String("before", "", "place before this sibling sqid")
	f.Bool("first", false, "place first among siblings")
	f.Bool("last", false, "place last among siblings (default)")
	c.MarkFlagsMutuallyExclusive("after", "before", "first", "last")
}

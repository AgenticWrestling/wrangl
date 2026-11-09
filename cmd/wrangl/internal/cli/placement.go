package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/placement"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func printNeed(w io.Writer, n *proto.NeedView) {
	if n == nil {
		return
	}
	src := func(f string) string {
		if s := n.Source[f]; s != "" {
			return " (" + s + ")"
		}
		return ""
	}
	fmt.Fprintf(w, "need\t%s%s · %s%s · %s%s\n", n.VCPU, src("vcpu"), n.RAM, src("ram"), n.Disk, src("disk"))
}

func placementCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{Use: "placement", Short: "Resource placement settings (default resource needs)"}
	show := &cobra.Command{
		Use: "show", Short: "Show the default resource needs every task inherits", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			v, err := c.Placement(cmd.Context())
			if err != nil {
				return err
			}
			return emit(cmd, g, v, func(w io.Writer) { printNeed(w, &v.Need) })
		},
	}
	set := &cobra.Command{
		Use: "set", Short: "Set the default resource needs: --need vcpu=1 --need ram=2GiB --need disk=5GiB", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			needs, _ := cmd.Flags().GetStringSlice("need")
			patch, err := placement.ParsePatch(needs)
			if err != nil {
				return err
			}
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			v, err := c.SetPlacement(cmd.Context(), proto.PlacementPatch{Need: patch})
			if err != nil {
				return err
			}
			return emit(cmd, g, v, func(w io.Writer) { printNeed(w, &v.Need) })
		},
	}
	set.Flags().StringSlice("need", nil, "resource need FIELD=VALUE (vcpu=1, ram=2GiB, disk=5GiB; repeatable)")
	cmd.AddCommand(show, set)
	return cmd
}

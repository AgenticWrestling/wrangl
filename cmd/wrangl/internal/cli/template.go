package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func templateCmd(g *Global) *cobra.Command {
	var file string
	create := &cobra.Command{
		Use: "create", Short: "Create a runner template from YAML", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			t, err := c.CreateTemplate(cmd.Context(), body)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created template %s (%s, %d vCPU, %d MiB, %d GiB, idle timeout %s)\n",
				t.Name, t.Arch, t.MinVCPU, t.MinRAMMiB, t.MinDiskGiB, time.Duration(t.IdleTimeoutS)*time.Second)
			return nil
		},
	}
	create.Flags().StringVarP(&file, "file", "f", "", "template YAML (required)")
	create.MarkFlagRequired("file")

	list := &cobra.Command{
		Use: "list", Short: "List templates", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			ts, err := c.Templates(cmd.Context())
			if err != nil {
				return err
			}
			return emit(cmd, g, ts, func(w io.Writer) {
				fmt.Fprintln(w, "NAME\tARCH\tVCPU\tRAM\tDISK\tTYPES\tMAX\tIDLE\tVERSION")
				for _, t := range ts {
					types := strings.Join(t.InstanceTypes, ",")
					if types == "" {
						types = "auto"
					}
					fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%dGiB\t%s\t%d\t%s\t%d\n", t.Name, t.Arch, t.MinVCPU, mib(t.MinRAMMiB),
						t.MinDiskGiB, types, t.MaxRunners, time.Duration(t.IdleTimeoutS)*time.Second, t.Version)
				}
			})
		},
	}
	show := &cobra.Command{
		Use: "show <name>", Short: "Show a template", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			t, err := c.Template(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, g, t, func(w io.Writer) {
				fmt.Fprintf(w, "name\t%s\narch\t%s\nminimum\t%d vCPU, %d MiB, %d GiB\ninstance types\t%v\nami\t%s\nmax runners\t%d\nidle timeout\t%s\nversion\t%d\n",
					t.Name, t.Arch, t.MinVCPU, t.MinRAMMiB, t.MinDiskGiB, t.InstanceTypes, t.AMI, t.MaxRunners,
					time.Duration(t.IdleTimeoutS)*time.Second, t.Version)
				fmt.Fprintf(w, "mise.toml\t\n%s\nagent\t%s\n", t.MiseTOML, t.Agent)
			})
		},
	}
	del := &cobra.Command{
		Use: "delete <name>", Short: "Delete an unused template", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			return c.DeleteTemplate(cmd.Context(), args[0])
		},
	}
	edit := leaf("edit <name>", "Replace a template (bumps its version)", "M3", cobra.ExactArgs(1))
	edit.Flags().StringP("file", "f", "", "template YAML (default: open $EDITOR)")
	return group("template", "Runner templates (mise config, specs, agent)", create, list, show, edit, del)
}

// mib renders a MiB quantity as GiB when it is a whole number of GiB.
func mib(v int64) string {
	if v >= 1024 && v%1024 == 0 {
		return fmt.Sprintf("%dGiB", v/1024)
	}
	return fmt.Sprintf("%dMiB", v)
}

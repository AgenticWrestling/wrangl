package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
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
	var editFile string
	edit := &cobra.Command{
		Use:   "edit <name>",
		Short: "Replace a template (bumps its version; runners re-apply it when idle)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			var body []byte
			if editFile != "" {
				if body, err = os.ReadFile(editFile); err != nil {
					return err
				}
			} else {
				cur, err := c.Template(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if body, err = editInEditor(TemplateYAML(cur)); err != nil {
					return err
				}
			}
			t, err := c.UpdateTemplate(cmd.Context(), args[0], body)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "template %s is now version %d\n", t.Name, t.Version)
			return nil
		},
	}
	edit.Flags().StringVarP(&editFile, "file", "f", "", "template YAML (default: open $EDITOR on the current definition)")
	return group("template", "Runner templates (mise config, specs, agent)", create, list, show, edit, del)
}

// TemplateYAML renders a template back into the file format `template create` accepts.
func TemplateYAML(t proto.TemplateView) []byte {
	var agent any
	json.Unmarshal(t.Agent, &agent)
	doc := map[string]any{
		"name": t.Name, "arch": t.Arch,
		"min":            map[string]any{"vcpu": t.MinVCPU, "ram": mib(t.MinRAMMiB), "disk": fmt.Sprintf("%dGiB", t.MinDiskGiB)},
		"instance_types": t.InstanceTypes, "ami": t.AMI, "max_runners": t.MaxRunners, "max_sessions": t.MaxSessions,
		"idle_timeout": (time.Duration(t.IdleTimeoutS) * time.Second).String(),
		"mise_toml":    t.MiseTOML,
	}
	if t.Description != "" {
		doc["description"] = t.Description
	}
	if t.SetupScript != "" {
		doc["setup_script"] = t.SetupScript
	}
	if m, ok := agent.(map[string]any); ok && len(m) > 0 {
		doc["agent"] = agent
	}
	b, _ := yaml.Marshal(doc)
	return b
}

func editInEditor(initial []byte) ([]byte, error) {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	f, err := os.CreateTemp("", "wrangl-template-*.yaml")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(initial); err != nil {
		return nil, err
	}
	f.Close()
	cmd := exec.Command("/bin/sh", "-c", editor+` "$1"`, "sh", f.Name())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("editor: %w", err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		return nil, err
	}
	if string(b) == string(initial) {
		return nil, fmt.Errorf("no changes")
	}
	return b, nil
}

// mib renders a MiB quantity as GiB when it is a whole number of GiB.
func mib(v int64) string {
	if v >= 1024 && v%1024 == 0 {
		return fmt.Sprintf("%dGiB", v/1024)
	}
	return fmt.Sprintf("%dMiB", v)
}

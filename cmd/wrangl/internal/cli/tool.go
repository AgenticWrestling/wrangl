package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/client"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/toolkdl"
)

// readKDL reads a tool file ("-" is stdin) and validates it locally so mistakes surface before
// the round trip.
func readKDL(cmd *cobra.Command, path string) (string, *toolkdl.Definition, error) {
	var b []byte
	var err error
	if path == "-" {
		b, err = io.ReadAll(cmd.InOrStdin())
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return "", nil, err
	}
	d, err := toolkdl.Parse(b)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", path, err)
	}
	return string(b), d, nil
}

func toolCmd(g *Global) *cobra.Command {
	var file, project string
	var force bool
	list := &cobra.Command{
		Use: "list", Short: "List library tools (or only those enabled on --project)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			ts, err := c.Tools(cmd.Context())
			if project != "" {
				ts, err = c.ProjectTools(cmd.Context(), project)
			}
			if err != nil {
				return err
			}
			return emit(cmd, g, ts, func(w io.Writer) {
				fmt.Fprintln(w, "NAME\tKIND\tEFFECT\tRUN KINDS\tENABLED ON\tVERSION")
				for _, t := range ts {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\n", t.Name, t.Kind, t.Effect, strings.Join(t.RunKinds, ","), dash(strings.Join(t.Projects, ",")), t.Version)
				}
			})
		},
	}
	list.Flags().StringVar(&project, "project", "", "only tools enabled on this project")
	show := &cobra.Command{
		Use: "show <name>", Short: "Show a tool's KDL source and the declaration agents see", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			t, err := c.Tool(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, g, t, func(w io.Writer) {
				fmt.Fprintf(w, "name\t%s\nkind\t%s\neffect\t%s\nversion\t%d\nenabled on\t%s\n", t.Name, t.Kind, t.Effect, t.Version, dash(strings.Join(t.Projects, ",")))
				fmt.Fprintf(w, "\n%s\n", strings.TrimRight(t.KDL, "\n"))
			})
		},
	}
	add := &cobra.Command{
		Use: "add -f tool.kdl", Short: "Add a tool to the library from a KDL v2 file (- reads stdin)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			src, _, err := readKDL(cmd, file)
			if err != nil {
				return err
			}
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			t, err := c.AddTool(cmd.Context(), src)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added tool %s; enable it with `wrangl tool enable %s --project NAME`\n", t.Name, t.Name)
			return nil
		},
	}
	add.Flags().StringVarP(&file, "file", "f", "", "tool definition, KDL v2 (required)")
	add.MarkFlagRequired("file")
	edit := &cobra.Command{
		Use: "edit <name>", Short: "Replace a tool's definition from -f, or in $EDITOR without it", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			var src string
			if file != "" {
				if src, _, err = readKDL(cmd, file); err != nil {
					return err
				}
			} else if src, err = editToolInEditor(cmd, c, args[0]); err != nil {
				return err
			}
			t, err := c.UpdateTool(cmd.Context(), args[0], src)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "updated tool %s (v%d); runs assigned from now on see it\n", t.Name, t.Version)
			return nil
		},
	}
	edit.Flags().StringVarP(&file, "file", "f", "", "new definition, KDL v2 (default: open $EDITOR)")
	rm := &cobra.Command{
		Use: "rm <name>", Short: "Remove a tool from the library", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			if err := c.RemoveTool(cmd.Context(), args[0], force); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed tool %s\n", args[0])
			return nil
		},
	}
	rm.Flags().BoolVar(&force, "force", false, "remove even while enabled on projects")
	toggle := func(on bool) *cobra.Command {
		use, short, verb := "enable <name> --project P", "Enable a library tool for a project", "enabled"
		if !on {
			use, short, verb = "disable <name> --project P", "Disable a tool for a project", "disabled"
		}
		c := &cobra.Command{
			Use: use, Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cl, err := apiClient(g)
				if err != nil {
					return err
				}
				t, err := cl.EnableTool(cmd.Context(), project, args[0], on)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "%s %s on %s\n", verb, args[0], project)
				for _, sc := range t.Secrets {
					if !sc.Set {
						fmt.Fprintf(out, "missing secret %s: set it with `wrangl tool secret set %s %s` (calls fail until then)\n", sc.Name, project, sc.Name)
					}
				}
				return nil
			},
		}
		c.Flags().StringVar(&project, "project", "", "project slug (required)")
		c.MarkFlagRequired("project")
		return c
	}
	var input string
	test := &cobra.Command{
		Use: "test (-f tool.kdl | <name> --project P [--input JSON])", Args: cobra.MaximumNArgs(1),
		Short: "Validate a tool file locally, or call a tool enabled on a project with sample input",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if file != "" {
				if len(args) > 0 {
					return fmt.Errorf("give either -f FILE or a tool name, not both")
				}
				_, d, err := readKDL(cmd, file)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "ok: %s (%s, effect %s, run kinds %s, %d params)\n", d.Name, d.Kind, d.Effect, strings.Join(d.RunKinds, ","), len(d.Params))
				if names := d.SecretNames(); len(names) > 0 {
					fmt.Fprintf(out, "secrets referenced: %s\n", strings.Join(names, ", "))
				}
				return nil
			}
			if len(args) != 1 || project == "" {
				return fmt.Errorf("give -f FILE, or a tool name with --project")
			}
			var raw json.RawMessage
			if input != "" {
				if !json.Valid([]byte(input)) {
					return fmt.Errorf("--input must be a JSON object")
				}
				raw = json.RawMessage(input)
			}
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			return c.TestTool(cmd.Context(), project, args[0], raw)
		},
	}
	test.Flags().StringVarP(&file, "file", "f", "", "validate this KDL file without contacting the controller")
	test.Flags().StringVar(&project, "project", "", "project the tool is enabled on")
	test.Flags().StringVar(&input, "input", "", "sample input as a JSON object")
	return group("tool", "Manage the system-wide custom tool library and enable tools per project", list, show, add, edit, rm, toggle(true), toggle(false), test, toolSecretCmd(g))
}

// toolSecretCmd sets and reports per-project tool secrets. Values are write-only.
func toolSecretCmd(g *Global) *cobra.Command {
	set := &cobra.Command{
		Use: "set <project> <name>", Short: "Store a tool secret for a project; the value is read from stdin (prompted without echo on a terminal)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := readSecretValue(cmd)
			if err != nil {
				return err
			}
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			if err := c.SetToolSecret(cmd.Context(), args[0], args[1], v); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "stored tool secret %s for %s\n", args[1], args[0])
			return nil
		},
	}
	status := &cobra.Command{
		Use: "status <project> <tool>", Short: "Show set/missing for each secret a tool declares in a project (never values)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			ss, err := c.ToolSecrets(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return emit(cmd, g, ss, func(w io.Writer) {
				fmt.Fprintln(w, "SECRET\tENV\tAS\tSTATUS")
				for _, s := range ss {
					st := "missing"
					if s.Set {
						st = "set"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Name, dash(s.Env), dash(s.As), st)
				}
			})
		},
	}
	return group("secret", "Per-project tool secrets (write-only)", set, status)
}

// readSecretValue reads the value from stdin, prompting without echo when stdin is a terminal.
func readSecretValue(cmd *cobra.Command) (string, error) {
	if f, ok := cmd.InOrStdin().(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(cmd.ErrOrStderr(), "Value (hidden): ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(cmd.ErrOrStderr())
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 64<<10+1))
	if err != nil {
		return "", err
	}
	if len(b) > 64<<10 {
		return "", fmt.Errorf("secret is larger than 64 KiB")
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("empty secret value on stdin")
	}
	return v, nil
}

// editToolInEditor fetches the tool's source, lets the owner edit it in $VISUAL/$EDITOR and
// returns the validated result.
func editToolInEditor(cmd *cobra.Command, c *client.Client, name string) (string, error) {
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	if ed == "" {
		return "", fmt.Errorf("set $EDITOR or pass -f FILE")
	}
	t, err := c.Tool(cmd.Context(), name)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "wrangl-tool-*.kdl")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(t.KDL); err != nil {
		return "", err
	}
	f.Close()
	parts := strings.Fields(ed)
	ec := exec.CommandContext(cmd.Context(), parts[0], append(parts[1:], f.Name())...)
	ec.Stdin, ec.Stdout, ec.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := ec.Run(); err != nil {
		return "", fmt.Errorf("editor: %w", err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		return "", err
	}
	if string(b) == t.KDL {
		return "", fmt.Errorf("no changes")
	}
	if _, err := toolkdl.Parse(b); err != nil {
		return "", err
	}
	return string(b), nil
}

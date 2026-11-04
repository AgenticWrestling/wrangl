package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// specFromFile reads an agent spec (YAML or JSON) and returns it as JSON.
func specFromFile(path string) (json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return json.Marshal(v)
}

func agentDefCmd(g *Global) *cobra.Command {
	var file, desc string
	list := &cobra.Command{
		Use: "list", Short: "List agent definitions and the templates that allow them", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			as, err := c.AgentDefs(cmd.Context())
			if err != nil {
				return err
			}
			return emit(cmd, g, as, func(w io.Writer) {
				fmt.Fprintln(w, "NAME\tVERSION\tALLOWED ON")
				for _, a := range as {
					fmt.Fprintf(w, "%s\t%d\t%s\n", a.Name, a.Version, dash(strings.Join(a.Templates, ",")))
				}
			})
		},
	}
	show := &cobra.Command{
		Use: "show <name>", Short: "Show an agent definition", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			a, err := c.AgentDef(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, g, a, func(w io.Writer) {
				fmt.Fprintf(w, "name\t%s\nversion\t%d\nallowed on\t%s\n", a.Name, a.Version, dash(strings.Join(a.Templates, ",")))
				var v any
				json.Unmarshal(a.Spec, &v)
				out, _ := yaml.Marshal(v)
				fmt.Fprintf(w, "\n%s", out)
			})
		},
	}
	create := &cobra.Command{
		Use: "create <name>", Short: "Create an agent definition from a YAML/JSON spec (start, resume, files, secrets…)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := specFromFile(file)
			if err != nil {
				return err
			}
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			a, err := c.CreateAgentDef(cmd.Context(), proto.AgentRequest{Name: args[0], Description: desc, Spec: spec})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created agent %s; allow it on a template with `wrangl agent allow %s --template NAME`\n", a.Name, a.Name)
			return nil
		},
	}
	edit := &cobra.Command{
		Use: "edit <name>", Short: "Replace an agent definition's spec", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := specFromFile(file)
			if err != nil {
				return err
			}
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			a, err := c.UpdateAgentDef(cmd.Context(), args[0], proto.AgentRequest{Description: desc, Spec: spec})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "updated agent %s (v%d); runs assigned from now on use it\n", a.Name, a.Version)
			return nil
		},
	}
	for _, c := range []*cobra.Command{create, edit} {
		c.Flags().StringVarP(&file, "file", "f", "", "agent spec, YAML or JSON (required)")
		c.Flags().StringVar(&desc, "description", "", "description")
		c.MarkFlagRequired("file")
	}
	del := &cobra.Command{
		Use: "delete <name>", Short: "Delete an agent definition", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			return c.DeleteAgentDef(cmd.Context(), args[0])
		},
	}
	var tmpl string
	allow := func(on bool) *cobra.Command {
		use, short := "allow <agent>", "Allow an agent on a template"
		if !on {
			use, short = "disallow <agent>", "Stop allowing an agent on a template"
		}
		c := &cobra.Command{
			Use: use, Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cl, err := apiClient(g)
				if err != nil {
					return err
				}
				return cl.AllowAgent(cmd.Context(), args[0], tmpl, on)
			},
		}
		c.Flags().StringVar(&tmpl, "template", "", "template name (required)")
		c.MarkFlagRequired("template")
		return c
	}
	return group("agent", "Manage named agent definitions (claude, codex, …) and where they may run", list, show, create, edit, del, allow(true), allow(false), agentAuthCmd(g))
}

// lastLine returns the last non-empty line of s, trimmed: `claude setup-token` prints some prose
// and then the token.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func secretCmd(g *Global) *cobra.Command {
	set := &cobra.Command{
		Use: "set <name>", Short: "Store a secret on the controller, read from stdin (e.g. `claude setup-token | wrangl secret set claude_oauth_token`)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 64<<10))
			if err != nil {
				return err
			}
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			if err := c.SetSecret(cmd.Context(), args[0], strings.TrimSpace(string(b))); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "stored secret %s\n", args[0])
			return nil
		},
	}
	list := &cobra.Command{
		Use: "list", Short: "List secret names (never values)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			names, err := c.Secrets(cmd.Context())
			if err != nil {
				return err
			}
			return emit(cmd, g, names, func(w io.Writer) {
				for _, n := range names {
					fmt.Fprintln(w, n)
				}
			})
		},
	}
	return group("secret", "Manage controller secrets over the API (values are write-only)", set, list)
}

// agentAuthCmd walks through collecting an agent's credential.
func agentAuthCmd(g *Global) *cobra.Command {
	return &cobra.Command{
		Use:   "auth <agent>",
		Short: "Collect an agent's credential: shows the command to run, then reads the token (last line of stdin) and stores it",
		Long: "Shows how the agent's credential is collected (its `auth` section; for Claude: `claude setup-token`), then reads the\n" +
			"token from stdin and stores it as the agent's secret. Pipe the command in, or paste at the prompt:\n\n" +
			"    claude setup-token | wrangl agent auth claude",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			a, err := c.AgentDef(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			var spec proto.AgentSpec
			json.Unmarshal(a.Spec, &spec)
			secret, command, note := "", "", ""
			if spec.Auth != nil {
				secret, command, note = spec.Auth.Secret, spec.Auth.Command, spec.Auth.Note
			}
			if secret == "" {
				for env, s := range spec.Secrets {
					if env == "CLAUDE_CODE_OAUTH_TOKEN" {
						secret, command = s, "claude setup-token"
					}
				}
			}
			if secret == "" {
				return fmt.Errorf("agent %s documents no credential to collect (add an `auth:` section to its spec)", a.Name)
			}
			errw := cmd.ErrOrStderr()
			if command != "" {
				fmt.Fprintf(errw, "Run this where %s is installed and logged in:\n    %s\n", a.Name, command)
			}
			if note != "" {
				fmt.Fprintf(errw, "(%s)\n", note)
			}
			fmt.Fprintf(errw, "Paste the token, then Ctrl-D (stored as secret %q): ", secret)
			b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 64<<10))
			if err != nil {
				return err
			}
			tok := lastLine(string(b))
			if tok == "" {
				return fmt.Errorf("no token read")
			}
			if err := c.SetSecret(cmd.Context(), secret, tok); err != nil {
				return err
			}
			fmt.Fprintf(errw, "\nstored secret %s for agent %s\n", secret, a.Name)
			return nil
		},
	}
}

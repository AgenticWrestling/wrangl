package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/nreid/devhost/cmd/wrangl/internal/config"
)

// clientConfigPath resolves the client config file: --config > WRANGL_CONFIG > default.
func clientConfigPath(g *Global) (string, error) {
	def, err := config.DefaultClientPath()
	if err != nil && g.Config == "" {
		return "", err
	}
	return config.Resolve(g.Config, "WRANGL_CONFIG", "", def), nil
}

func configCmd(g *Global) *cobra.Command {
	use := &cobra.Command{
		Use:   "use <url>",
		Short: "Set the controller this client talks to (https://… or unix:///…)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := clientConfigPath(g)
			if err != nil {
				return err
			}
			c, err := config.LoadClient(p)
			if err != nil {
				return err
			}
			c.Controller = args[0]
			if err := config.SaveClient(p, c); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "controller %s (saved to %s)\n", c.Controller, p)
			return nil
		},
	}
	show := &cobra.Command{
		Use:   "show",
		Short: "Show the effective client config (secrets redacted)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := clientConfigPath(g)
			if err != nil {
				return err
			}
			c, err := config.LoadClient(p)
			if err != nil {
				return err
			}
			c.Controller = config.Resolve(g.Controller, "WRANGL_CONTROLLER", c.Controller, config.DefaultLocalController)
			r := c.Redacted()
			out := cmd.OutOrStdout()
			switch g.Output {
			case "json":
				e := json.NewEncoder(out)
				e.SetIndent("", "  ")
				return e.Encode(r)
			case "yaml":
				return yaml.NewEncoder(out).Encode(r)
			}
			fmt.Fprintf(out, "config      %s\ncontroller  %s\n", p, r.Controller)
			if r.CFServiceToken != nil {
				fmt.Fprintf(out, "cf token    %s (secret %s)\n", r.CFServiceToken.ID, r.CFServiceToken.Secret)
			}
			return nil
		},
	}
	return group("config", "Client configuration", use, show)
}

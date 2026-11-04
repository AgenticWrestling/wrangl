package cli

import (
	"encoding/json"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/client"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/config"
)

type clientT = client.Client

// apiClient resolves the controller (flag > env > client config > local socket).
func apiClient(g *Global) (*client.Client, error) {
	p, err := clientConfigPath(g)
	if err != nil {
		return nil, err
	}
	c, err := config.LoadClient(p)
	if err != nil {
		return nil, err
	}
	url := config.Resolve(g.Controller, "WRANGL_CONTROLLER", c.Controller, config.DefaultLocalController)
	return client.New(url, c.CFServiceToken)
}

// emit prints v as JSON/YAML when asked, otherwise calls table.
func emit(cmd *cobra.Command, g *Global, v any, table func(w io.Writer)) error {
	out := cmd.OutOrStdout()
	switch g.Output {
	case "json":
		e := json.NewEncoder(out)
		e.SetIndent("", "  ")
		return e.Encode(v)
	case "yaml":
		return yaml.NewEncoder(out).Encode(v)
	}
	if table == nil {
		e := json.NewEncoder(out)
		e.SetIndent("", "  ")
		return e.Encode(v)
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	table(tw)
	return tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

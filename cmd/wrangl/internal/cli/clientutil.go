package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/nreid/devhost/cmd/wrangl/internal/client"
	"github.com/nreid/devhost/cmd/wrangl/internal/config"
)

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
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	table(tw)
	return tw.Flush()
}

// ago renders an RFC 3339 timestamp as a short relative duration.
func ago(ts string, now time.Time) string {
	if ts == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts
	}
	d := now.Sub(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%.1fh", d.Hours())
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

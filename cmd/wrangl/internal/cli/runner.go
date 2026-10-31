package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/nreid/devhost/cmd/wrangl/internal/runner"
)

func runnerCmd(g *Global) *cobra.Command {
	var bo runner.BootstrapOptions
	bootstrap := &cobra.Command{
		Use:   "bootstrap",
		Short: "Enrol this instance and install the runner daemon (run once, as root, by cloud-init)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			bo.Controller = g.Controller
			if bo.Controller == "" {
				return errors.New("--controller http://<controller-private-ip>:4223 is required")
			}
			bo.Log = cmd.OutOrStdout()
			_, err := runner.Bootstrap(cmd.Context(), bo)
			return err
		},
	}
	bf := bootstrap.Flags()
	bf.StringVar(&bo.Token, "token", "", "one-time enrolment token (required)")
	bf.StringVar(&bo.SHA256, "sha256", "", "expected sha256 of this binary")
	bootstrap.MarkFlagRequired("token")
	for _, h := range []struct {
		name, usage string
		p           any
	}{
		{"root", "filesystem prefix (tests)", &bo.Root},
		{"imds-url", "IMDS endpoint (tests)", &bo.IMDSURL},
		{"no-systemd", "do not install or start the systemd unit", &bo.NoSystemd},
		{"no-user", "do not create the wrangl user or chown files", &bo.NoUser},
	} {
		switch p := h.p.(type) {
		case *string:
			bf.StringVar(p, h.name, "", h.usage)
		case *bool:
			bf.BoolVar(p, h.name, false, h.usage)
		}
		bf.MarkHidden(h.name)
	}

	// Runner commands read the runner config: global --config, else /etc/wrangl/runner.yaml.
	runnerConfig := func() (*runner.Config, error) {
		path := g.Config
		if path == "" {
			path = runner.DefaultConfigPath
		}
		return runner.LoadConfig(path)
	}
	run := &cobra.Command{
		Use: "run", Short: "Runner daemon (systemd ExecStart)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := runnerConfig()
			if err != nil {
				return err
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("runner", cfg.Name)
			return runner.Run(cmd.Context(), cfg, runner.DaemonOptions{Log: log, InstallMise: runner.DefaultMiseInstaller})
		},
	}
	status := &cobra.Command{
		Use: "status", Short: "Local runner status and a round trip to the controller", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := runnerConfig()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "runner      %s (id %d)\ncontroller  %s\nnats        %v\n", cfg.Name, cfg.RunnerID, cfg.Controller, cfg.NATSURLs)
			rtt, err := runner.Status(cmd.Context(), cfg)
			if err != nil {
				fmt.Fprintf(out, "link        DOWN: %v\n", err)
				return err
			}
			fmt.Fprintf(out, "link        up, rtt %s\n", rtt)
			return nil
		},
	}
	hook := leaf("hook <event>", "Agent hook entrypoint: forwards stdin JSON to the daemon", "M5", cobra.ExactArgs(1))
	hook.Hidden = true
	hook.Flags().String("continue-format", "", "print an agent-specific continue response (interactive drive)")
	mcp := leaf("mcp", "stdio MCP server for the agent", "M5", cobra.NoArgs)
	mcp.Hidden = true

	return group("runner", "Runner mode (runs on a runner instance)", bootstrap, run, hook, mcp, status)
}

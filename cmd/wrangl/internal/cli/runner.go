package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner"
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
	bf.BoolVar(&bo.Host, "host", false, "enrol THIS machine (the controller host) as a permanent runner under the current user; installs a systemd --user unit")
	bf.StringVar(&bo.HostAgentUser, "agent-user", "", "with --host: run agents as this dedicated user (e.g. wrangl-agent), created if missing; needs root, installs a system unit and never touches your home")
	bf.IntVar(&bo.SliceCPUWeight, "slice-cpu-weight", 0, "with --agent-user: CPUWeight of the agent slice (default 50; 100 is an equal share)")
	bf.StringVar(&bo.SliceMemoryHigh, "slice-memory-high", "", "with --agent-user: MemoryHigh of the agent slice (default 60%)")
	bf.StringVar(&bo.SliceMemoryMax, "slice-memory-max", "", "with --agent-user: MemoryMax of the agent slice (default 75%)")
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
			up, err := runner.DefaultSelfUpgrade()
			if err != nil {
				log.Warn("self-upgrade off", "err", err)
				up = nil
			}
			return runner.Run(cmd.Context(), cfg, runner.DaemonOptions{Log: log, InstallMise: runner.DefaultMiseInstaller, Upgrade: up})
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
	var contFmt string
	hook := &cobra.Command{
		Use: "hook <event>", Short: "Agent hook entrypoint: forwards stdin JSON to the daemon", Args: cobra.ExactArgs(1), Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			payload, _ := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
			if err := runner.SendHook(cmd.Context(), args[0], payload, contFmt, cmd.OutOrStdout()); err != nil {
				// Never block or fail the agent because of a hook.
				fmt.Fprintln(cmd.ErrOrStderr(), "wrangl hook:", err)
			}
			return nil
		},
	}
	hook.Flags().StringVar(&contFmt, "continue-format", "", "print an agent-specific continue response (interactive drive): claude, codex, agy, gemini, text")
	mcpCmd := &cobra.Command{
		Use: "mcp", Short: "stdio MCP server for the agent (work-stack tools)", Args: cobra.NoArgs, Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runner.ServeMCP(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), runner.Version)
		},
	}
	tool := &cobra.Command{
		Use: "tool <name> [json]", Short: "Call a work-stack tool from a script inside a run", Args: cobra.RangeArgs(1, 2), Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var body []byte
			if len(args) == 2 {
				body = []byte(args[1])
			}
			out, err := runner.CallTool(cmd.Context(), args[0], body)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(out))
			return nil
		},
	}

	return group("runner", "Runner mode (runs on a runner instance)", bootstrap, run, hook, mcpCmd, tool, status)
}

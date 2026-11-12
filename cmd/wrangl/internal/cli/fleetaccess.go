package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/awsx/ssmtunnel"
)

// regionFromAZ turns "us-west-2b" into "us-west-2".
func regionFromAZ(az string) string {
	if n := len(az); n > 0 && az[n-1] >= 'a' && az[n-1] <= 'z' {
		return az[:n-1]
	}
	return az
}

// runnerInstance asks the controller for the runner's EC2 instance and returns the target
// argument ("region/i-...") for the awsx resolver.
func runnerInstance(ctx context.Context, g *Global, name string) (string, error) {
	c, err := apiClient(g)
	if err != nil {
		return "", err
	}
	r, err := c.Runner(ctx, name)
	if err != nil {
		return "", err
	}
	if r.InstanceID == "" {
		return "", fmt.Errorf("runner %s has no EC2 instance (a host runner or a box adopted without instance metadata)", name)
	}
	if reg := regionFromAZ(r.AZ); reg != "" {
		return reg + "/" + r.InstanceID, nil
	}
	return r.InstanceID, nil
}

func fleetConnect(g *Global) *cobra.Command {
	o := &devOpts{}
	c := &cobra.Command{
		Use: "connect <runner>", Short: "Open an SSM shell on a runner's instance (debugging; the web terminal is the usual way)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := runnerInstance(cmd.Context(), g, args[0])
			if err != nil {
				return err
			}
			s, i, _, err := o.resolver().Resolve(cmd.Context(), t)
			if err != nil {
				return err
			}
			return execAWS(sessionArgs(s, i)...)
		},
	}
	c.Flags().StringVar(&o.profile, "profile", "", "AWS profile (env AWS_PROFILE)")
	c.Flags().StringVar(&o.region, "region", "", "AWS region (default: the runner's)")
	return c
}

func fleetTunnel(g *Global) *cobra.Command {
	o := &devOpts{}
	c := &cobra.Command{
		Use: "tunnel <runner> RPORT[:LPORT]", Short: "Forward a local port to a port on a runner's instance over SSM", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := ssmtunnel.ParsePortSpec(args[1])
			if err != nil {
				return err
			}
			t, err := runnerInstance(cmd.Context(), g, args[0])
			if err != nil {
				return err
			}
			s, i, _, err := o.resolver().Resolve(cmd.Context(), t)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			ready := func() {
				fmt.Fprintf(cmd.ErrOrStderr(), "Tunnel ready: http://localhost:%d -> %s:%d (Ctrl-C closes it)\n", spec.Local, args[0], spec.Remote)
			}
			return ssmtunnel.Run(ctx, ssmtunnel.Args(i.ID, s.Profile, i.Region, spec.Remote, spec.Local), spec.Local, 30*time.Second, ready)
		},
	}
	c.Flags().StringVar(&o.profile, "profile", "", "AWS profile (env AWS_PROFILE)")
	c.Flags().StringVar(&o.region, "region", "", "AWS region (default: the runner's)")
	return c
}

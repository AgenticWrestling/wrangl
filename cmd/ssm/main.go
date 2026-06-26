package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	ssmaws "github.com/nreid/devhost/cmd/ssm/internal/aws"
	"github.com/nreid/devhost/cmd/ssm/internal/ui"
)

func main() {
	root := &cobra.Command{
		Use:   "ssm",
		Short: "Manage EC2 dev instances via SSM",
	}
	root.AddCommand(connectCmd(), tunnelCmd(), setupCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// resolveTarget picks a profile interactively, then resolves region/name arg
// to a running instance. If args is empty or no region/name provided, lists
// all running instances for interactive selection.
func resolveTarget(args []string, defaultRegion string) (string, *ssmaws.Instance, error) {
	profiles, err := ssmaws.ListProfiles()
	if err != nil {
		return "", nil, fmt.Errorf("listing profiles: %w", err)
	}
	profile, err := ui.PickProfile(profiles)
	if err != nil {
		return "", nil, err
	}

	ctx := context.Background()

	if len(args) > 0 {
		region, name, err := ssmaws.ParseRegionName(args[0])
		if err != nil {
			return "", nil, err
		}
		inst, err := ssmaws.ResolveInstance(ctx, profile, region, name)
		return profile, inst, err
	}

	if defaultRegion == "" {
		defaultRegion = "us-east-1"
	}
	instances, err := ssmaws.ListInstances(ctx, profile, defaultRegion)
	if err != nil {
		return "", nil, err
	}
	inst, err := ui.PickInstance(instances)
	return profile, inst, err
}

func connectCmd() *cobra.Command {
	return &cobra.Command{Use: "connect [region/name]", Short: "Open SSM terminal session", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
}

func tunnelCmd() *cobra.Command {
	return &cobra.Command{Use: "tunnel [region/name] <rport>[:<lport>]", Short: "SSM port-forward tunnel", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
}

func setupCmd() *cobra.Command {
	return &cobra.Command{Use: "setup [region/name]", Short: "Provision dev toolchain on instance", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
}

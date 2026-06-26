package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

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
	return &cobra.Command{
		Use:   "connect [region/name]",
		Short: "Open interactive SSM terminal session",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile, inst, err := resolveTarget(args, "")
			if err != nil {
				return err
			}
			execArgs := buildConnectArgs(inst.ID, inst.Region, profile)
			awsPath, err := exec.LookPath("aws")
			if err != nil {
				return fmt.Errorf("aws CLI not found in PATH: %w", err)
			}
			return syscall.Exec(awsPath, execArgs, os.Environ())
		},
	}
}

func buildConnectArgs(instanceID, region, profile string) []string {
	return []string{
		"aws", "ssm", "start-session",
		"--target", instanceID,
		"--region", region,
		"--profile", profile,
	}
}

func tunnelCmd() *cobra.Command {
	return &cobra.Command{Use: "tunnel [region/name] <rport>[:<lport>]", Short: "SSM port-forward tunnel", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
}

func setupCmd() *cobra.Command {
	return &cobra.Command{Use: "setup [region/name]", Short: "Provision dev toolchain on instance", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
}

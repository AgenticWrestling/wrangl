package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
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

func connectCmd() *cobra.Command {
	return &cobra.Command{Use: "connect [region/name]", Short: "Open SSM terminal session", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
}

func tunnelCmd() *cobra.Command {
	return &cobra.Command{Use: "tunnel [region/name] <rport>[:<lport>]", Short: "SSM port-forward tunnel", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
}

func setupCmd() *cobra.Command {
	return &cobra.Command{Use: "setup [region/name]", Short: "Provision dev toolchain on instance", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
}

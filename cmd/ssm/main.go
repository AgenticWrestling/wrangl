package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	ssmaws "github.com/nreid/devhost/cmd/ssm/internal/aws"
	"github.com/nreid/devhost/cmd/ssm/internal/cwlogs"
	"github.com/nreid/devhost/cmd/ssm/internal/eic"
	sshpkg "github.com/nreid/devhost/cmd/ssm/internal/ssh"
	"github.com/nreid/devhost/cmd/ssm/internal/provision"
	"github.com/nreid/devhost/cmd/ssm/internal/tunnel"
	"github.com/nreid/devhost/cmd/ssm/internal/ui"
	"github.com/nreid/devhost/cmd/ssm/internal/vpc"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	root := &cobra.Command{
		Use:           "ssm",
		Short:         "Manage EC2 dev instances via SSM",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(connectCmd(), tunnelCmd(), setupCmd())

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, friendlyError(err))
		os.Exit(1)
	}
}

// friendlyError maps noisy AWS SDK errors to actionable messages.
func friendlyError(err error) string {
	if isSSOExpired(err) {
		return "AWS SSO session expired. Run: aws sso login --profile <profile>"
	}
	return err.Error()
}

func isSSOExpired(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "InvalidGrantException") || strings.Contains(s, "refresh cached SSO token")
}

// refreshSSO runs `aws sso login --profile <profile>` interactively so the
// user can re-authenticate without leaving the tool.
func refreshSSO(profile string) error {
	awsPath, err := exec.LookPath("aws")
	if err != nil {
		return fmt.Errorf("aws CLI not found in PATH: %w", err)
	}
	cmd := exec.Command(awsPath, "sso", "login", "--profile", profile)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// resolveTarget picks a profile interactively, then resolves region/name arg
// to a running instance. If args is empty or no region/name provided, lists
// all running instances for interactive selection.
func resolveTarget(ctx context.Context, args []string, defaultRegion string) (string, *ssmaws.Instance, error) {
	profiles, err := ssmaws.ListProfiles()
	if err != nil {
		return "", nil, fmt.Errorf("listing profiles: %w", err)
	}
	profile, err := ui.PickProfile(profiles)
	if err != nil {
		return "", nil, err
	}

	// pickFromRegion lists all running instances in region and prompts the user.
	pickFromRegion := func(region string) (string, *ssmaws.Instance, error) {
		instances, err := ssmaws.ListInstances(ctx, profile, region)
		if err != nil {
			return "", nil, err
		}
		inst, err := ui.PickInstance(instances)
		return profile, inst, err
	}

	resolve := func() (string, *ssmaws.Instance, error) {
		if len(args) == 0 {
			// No args: list in default/fallback region.
			if defaultRegion == "" {
				defaultRegion = "us-east-1"
			}
			return pickFromRegion(defaultRegion)
		}

		arg := args[0]

		// "region/name" form — try exact resolve, fall back to picker on no match.
		if region, name, err := ssmaws.ParseRegionName(arg); err == nil {
			inst, err := ssmaws.ResolveInstance(ctx, profile, region, name)
			if err == nil {
				return profile, inst, nil
			}
			// No match or ambiguous — tell the user and let them pick.
			fmt.Fprintf(os.Stderr, "No running instance named %q in %s — pick one:\n", name, region)
			return pickFromRegion(region)
		}

		// "region" only (no slash) — list instances in that region.
		return pickFromRegion(arg)
	}

	p, inst, err := resolve()
	if isSSOExpired(err) {
		fmt.Fprintf(os.Stderr, "SSO session expired for profile %q. Refreshing...\n", profile)
		if loginErr := refreshSSO(profile); loginErr != nil {
			return "", nil, fmt.Errorf("sso login: %w", loginErr)
		}
		p, inst, err = resolve()
	}
	return p, inst, err
}

func connectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "connect [region[/name]]",
		Short: "Open interactive SSM terminal session",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile, inst, err := resolveTarget(cmd.Context(), args, "")
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
	var openBrowser bool
	cmd := &cobra.Command{
		Use:   "tunnel [region/name] <rport>[:<lport>]",
		Short: "SSM port-forward tunnel to a remote port",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Last arg is always the port spec; region/name is optional first arg.
			portArg := args[len(args)-1]
			targetArgs := args[:len(args)-1]

			spec, err := tunnel.ParsePortSpec(portArg)
			if err != nil {
				return err
			}

			profile, inst, err := resolveTarget(cmd.Context(), targetArgs, "")
			if err != nil {
				return err
			}

			tunnelArgs := buildTunnelArgs(inst.ID, inst.Region, profile, spec.RemotePort, spec.LocalPort)
			awsPath, err := exec.LookPath("aws")
			if err != nil {
				return fmt.Errorf("aws CLI not found in PATH: %w", err)
			}

			proc := exec.Command(awsPath, tunnelArgs[1:]...)
			proc.Stdin = os.Stdin
			proc.Stdout = os.Stdout
			proc.Stderr = os.Stderr
			if err := proc.Start(); err != nil {
				return fmt.Errorf("starting tunnel: %w", err)
			}

			if err := waitForPort(spec.LocalPort, 15); err != nil {
				proc.Process.Kill()
				return err
			}
			fmt.Fprintf(os.Stderr, "Tunnel ready: http://localhost:%d\n", spec.LocalPort)

			if openBrowser {
				exec.Command("open", fmt.Sprintf("http://localhost:%d", spec.LocalPort)).Start()
			}

			// Wait for Ctrl-C.
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt)
			<-sig
			fmt.Fprintln(os.Stderr, "\nClosing tunnel...")
			proc.Process.Kill()
			return proc.Wait()
		},
	}
	cmd.Flags().BoolVar(&openBrowser, "open", false, "Open browser when tunnel is ready")
	return cmd
}

func buildTunnelArgs(instanceID, region, profile string, remotePort, localPort int) []string {
	return []string{
		"aws", "ssm", "start-session",
		"--target", instanceID,
		"--region", region,
		"--profile", profile,
		"--document-name", "AWS-StartPortForwardingSession",
		"--parameters", fmt.Sprintf(`{"portNumber":["%d"],"localPortNumber":["%d"]}`, remotePort, localPort),
	}
}

func waitForPort(port, timeoutSecs int) error {
	deadline := time.Now().Add(time.Duration(timeoutSecs) * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("port %d not ready after %ds", port, timeoutSecs)
}

func setupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup [region[/name]]",
		Short: "Provision dev toolchain on instance",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile, inst, err := resolveTarget(cmd.Context(), args, "")
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			// Phase 1a: EIC Endpoint
			if err := ui.Spinner("Ensuring EIC endpoint in VPC...", func() error {
				return eic.EnsureEndpoint(ctx, profile, inst.Region, inst.VPCID, inst.SubnetID)
			}); err != nil {
				return fmt.Errorf("EIC endpoint: %w", err)
			}
			fmt.Println("✓ EIC endpoint ready")

			// Phase 1b: Security group
			if err := ui.Spinner("Ensuring SSH ingress rule...", func() error {
				return vpc.EnsureSSHIngressFromVPC(ctx, profile, inst.Region, inst.VPCID, inst.SecurityGroupIDs)
			}); err != nil {
				return fmt.Errorf("security group: %w", err)
			}
			fmt.Println("✓ Security group rule present")

			// Phase 1c: SSH keypair
			pubKey, err := sshpkg.EnsureKeyPair()
			if err != nil {
				return fmt.Errorf("SSH keypair: %w", err)
			}
			fmt.Println("✓ SSH keypair ready")

			// Phase 1d: Push public key via EIC
			if err := ui.Spinner("Pushing SSH public key via EIC...", func() error {
				return eic.PushPublicKey(ctx, profile, inst.Region, inst.ID, pubKey)
			}); err != nil {
				return fmt.Errorf("push SSH public key: %w", err)
			}
			fmt.Println("✓ SSH public key registered (valid 60s)")

			// Phase 1e: SSH config
			if err := sshpkg.UpsertHostBlock(inst.Name, inst.ID, inst.Region, profile); err != nil {
				return fmt.Errorf("SSH config: %w", err)
			}
			fmt.Printf("✓ ~/.ssh/config updated — connect with: ssh %s\n", inst.Name)

			// Phase 2: Remote provisioning
			fmt.Println("Sending provisioning script...")
			commandID, err := provision.Run(ctx, profile, inst.Region, inst.ID)
			if err != nil {
				return fmt.Errorf("provisioning: %w", err)
			}
			fmt.Printf("Command ID: %s\n", commandID)
			fmt.Println("Tailing CloudWatch Logs (Ctrl-C to detach)...")

			// Phase 3: Tail logs until done
			if err := cwlogs.Tail(ctx, profile, inst.Region, commandID, inst.ID, os.Stdout); err != nil {
				return fmt.Errorf("provisioning failed: %w", err)
			}

			return nil
		},
	}
}

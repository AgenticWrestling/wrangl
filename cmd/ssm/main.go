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

	ssmaws "github.com/agenticwrestling/wrangl/cmd/ssm/internal/aws"
	"github.com/agenticwrestling/wrangl/cmd/ssm/internal/cwlogs"
	"github.com/agenticwrestling/wrangl/cmd/ssm/internal/eic"
	"github.com/agenticwrestling/wrangl/cmd/ssm/internal/provision"
	sshpkg "github.com/agenticwrestling/wrangl/cmd/ssm/internal/ssh"
	"github.com/agenticwrestling/wrangl/cmd/ssm/internal/tunnel"
	"github.com/agenticwrestling/wrangl/cmd/ssm/internal/ui"
	"github.com/agenticwrestling/wrangl/cmd/ssm/internal/vpc"
	"github.com/spf13/cobra"
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
	root.AddCommand(connectCmd(), tunnelCmd(), setupCmd(), sshCmd(), startCmd())

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

// looksLikeRegion returns true if s matches AWS region format: xx-word-N
// e.g. "us-west-2", "eu-central-1", "ap-southeast-2"
func looksLikeRegion(s string) bool {
	parts := strings.SplitN(s, "-", 3)
	if len(parts) < 3 {
		return false
	}
	last := parts[len(parts)-1]
	for _, c := range last {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(last) > 0
}

func isSSOExpired(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "InvalidGrantException") ||
		strings.Contains(s, "refresh cached SSO token") ||
		strings.Contains(s, "login session has expired") ||
		strings.Contains(s, "please reauthenticate")
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
	profiles = ssmaws.SortProfilesByMRU(profiles)
	profile, err := ui.PickProfile(ctx, profiles)
	if err != nil {
		return "", nil, err
	}
	_ = ssmaws.RecordProfileUsage(profile)

	// Proactively refresh SSO token before any API call.
	if expired, _ := ssmaws.SSOTokenExpired(profile); expired {
		fmt.Fprintf(os.Stderr, "SSO session expired for profile %q. Refreshing...\n", profile)
		if loginErr := refreshSSO(profile); loginErr != nil {
			return "", nil, fmt.Errorf("sso login: %w", loginErr)
		}
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

		// No slash. Decide: does this look like a region ("us-west-2") or a name?
		if looksLikeRegion(arg) {
			return pickFromRegion(arg)
		}

		// Treat as an instance name in the default region.
		if defaultRegion == "" {
			defaultRegion = "us-east-1"
		}
		inst, err := ssmaws.ResolveInstance(ctx, profile, defaultRegion, arg)
		if err == nil {
			return profile, inst, nil
		}
		fmt.Fprintf(os.Stderr, "No running instance named %q in %s — pick one:\n", arg, defaultRegion)
		return pickFromRegion(defaultRegion)
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

func sshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ssh [region[/[user@]name]]",
		Short: "SSH into an instance via EIC (pushes key then execs ssh)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Extract user@name from the name part if present, then pass
			// the stripped args to resolveTarget.
			userExplicit := false
			osUser := "ec2-user"
			strippedArgs := args
			if len(args) == 1 {
				var stripped []string
				stripped, osUser = stripUserFromArg(args)
				if stripped[0] != args[0] {
					userExplicit = true
				}
				strippedArgs = stripped
			}

			// If a bare name was given (no region/), look up metadata from ~/.ssh/config.
			defaultRegion := ""
			knownProfile := ""
			if len(strippedArgs) == 1 && !strings.Contains(strippedArgs[0], "/") {
				if meta := sshpkg.LookupHost(strippedArgs[0]); meta.Region != "" {
					defaultRegion = meta.Region
					knownProfile = meta.Profile
					if !userExplicit && meta.User != "" {
						osUser = meta.User
					}
				}
			}

			// Skip profile picker if we already know the profile from ~/.ssh/config.
			var profile string
			var inst *ssmaws.Instance
			var err error
			if knownProfile != "" {
				inst, err = ssmaws.ResolveInstance(cmd.Context(), knownProfile, defaultRegion, strippedArgs[0])
				if err != nil {
					return err
				}
				profile = knownProfile
			} else {
				profile, inst, err = resolveTarget(cmd.Context(), strippedArgs, defaultRegion)
				if err != nil {
					return err
				}
			}
			ctx := cmd.Context()

			pubKey, err := sshpkg.EnsureKeyPair()
			if err != nil {
				return fmt.Errorf("SSH keypair: %w", err)
			}

			if err := eic.PushPublicKey(ctx, profile, inst.Region, inst.ID, osUser, pubKey); err != nil {
				return fmt.Errorf("push SSH public key: %w", err)
			}

			if err := sshpkg.UpsertHostBlock(inst.Name, inst.ID, inst.Region, profile, osUser); err != nil {
				return fmt.Errorf("SSH config: %w", err)
			}

			sshPath, err := exec.LookPath("ssh")
			if err != nil {
				return fmt.Errorf("ssh not found in PATH: %w", err)
			}
			return syscall.Exec(sshPath, []string{"ssh", inst.Name}, os.Environ())
		},
	}
}

// stripUserFromArg parses optional user@ prefix from the name part of a
// region/name or bare-name argument. Returns the stripped args and the user.
func stripUserFromArg(args []string) ([]string, string) {
	if len(args) == 0 {
		return args, "ec2-user"
	}
	arg := args[0]
	// Handle both "region/user@name" and "user@name"
	if region, rest, hasSlash := strings.Cut(arg, "/"); hasSlash {
		if user, name, hasAt := strings.Cut(rest, "@"); hasAt {
			return []string{region + "/" + name}, user
		}
	} else if user, name, hasAt := strings.Cut(arg, "@"); hasAt {
		return []string{name}, user
	}
	return args, "ec2-user"
}

func startCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start [region[/name]]",
		Short: "Start a stopped instance and wait until it's running",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			profiles, err := ssmaws.ListProfiles()
			if err != nil {
				return fmt.Errorf("listing profiles: %w", err)
			}
			profiles = ssmaws.SortProfilesByMRU(profiles)
			profile, err := ui.PickProfile(ctx, profiles)
			if err != nil {
				return err
			}
			_ = ssmaws.RecordProfileUsage(profile)

			if expired, _ := ssmaws.SSOTokenExpired(profile); expired {
				fmt.Fprintf(os.Stderr, "SSO session expired for profile %q. Refreshing...\n", profile)
				if loginErr := refreshSSO(profile); loginErr != nil {
					return fmt.Errorf("sso login: %w", loginErr)
				}
			}

			region := "us-east-1"
			if len(args) == 1 && looksLikeRegion(args[0]) {
				region = args[0]
			} else if len(args) == 1 {
				if r, _, err := ssmaws.ParseRegionName(args[0]); err == nil {
					region = r
				}
			}

			instances, err := ssmaws.ListStoppedInstances(ctx, profile, region)
			if err != nil {
				return err
			}
			if len(instances) == 0 {
				fmt.Fprintf(os.Stderr, "No stopped instances found in %s\n", region)
				return nil
			}

			inst, err := ui.PickInstance(instances)
			if err != nil {
				return err
			}

			return ui.Spinner(fmt.Sprintf("Starting %s...", inst.Name), func() error {
				if err := ssmaws.StartInstance(ctx, profile, inst.Region, inst.ID); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "\n✓ %s is running\n", inst.Name)
				return nil
			})
		},
	}
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

			// Phase 1b: Security group — VPC SSH rule
			if err := ui.Spinner("Ensuring SSH ingress rule...", func() error {
				return vpc.EnsureSSHIngressFromVPC(ctx, profile, inst.Region, inst.VPCID, inst.SecurityGroupIDs)
			}); err != nil {
				return fmt.Errorf("security group: %w", err)
			}
			fmt.Println("✓ Security group rule present")

			// Phase 1b2: Open all ports from caller's public IP
			var myIP string
			if err := ui.Spinner("Opening all ports from your public IP...", func() error {
				var e error
				myIP, e = vpc.EnsureMyIPIngress(ctx, profile, inst.Region, inst.SecurityGroupIDs)
				return e
			}); err != nil {
				return fmt.Errorf("my-IP ingress rule: %w", err)
			}
			fmt.Printf("✓ All ports open from %s\n", myIP)

			// Phase 1c: SSH keypair
			pubKey, err := sshpkg.EnsureKeyPair()
			if err != nil {
				return fmt.Errorf("SSH keypair: %w", err)
			}
			fmt.Println("✓ SSH keypair ready")

			// Phase 1d: Push public key via EIC
			if err := ui.Spinner("Pushing SSH public key via EIC...", func() error {
				return eic.PushPublicKey(ctx, profile, inst.Region, inst.ID, "ec2-user", pubKey)
			}); err != nil {
				return fmt.Errorf("push SSH public key: %w", err)
			}
			fmt.Println("✓ SSH public key registered (valid 60s)")

			// Phase 1e: SSH config
			if err := sshpkg.UpsertHostBlock(inst.Name, inst.ID, inst.Region, profile, "ec2-user"); err != nil {
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

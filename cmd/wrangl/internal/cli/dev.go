package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2instanceconnect"
	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/awsx"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/awsx/eic"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/awsx/ssmrun"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/awsx/ssmtunnel"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/devbox"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/runner"
)

// devOpts are the persistent flags of `wrangl dev`.
type devOpts struct{ profile, region string }

func (o *devOpts) resolver() *awsx.Resolver {
	return &awsx.Resolver{Profile: o.profile, Region: o.region}
}

// devCmd is `wrangl dev`: your own EC2 dev instances (connect, ssh, tunnel, start, stop, set up
// a toolchain with mise). It never talks to the controller, except `setup --template`.
func devCmd(g *Global) *cobra.Command {
	o := &devOpts{}
	cmds := []*cobra.Command{
		devList(o), devConnect(o), devSSH(o), devSSHConfig(o), devProxy(o), devTunnel(o),
		devStartStop(o, true), devStartStop(o, false), devSetup(g, o),
	}
	c := group("dev", "Your own EC2 dev instances: connect, ssh, tunnel, start/stop, set up a mise toolchain (no controller needed)", cmds...)
	c.PersistentFlags().StringVar(&o.profile, "profile", "", "AWS profile (env AWS_PROFILE; default: pick from ~/.aws/config)")
	c.PersistentFlags().StringVar(&o.region, "region", "", "AWS region (env AWS_REGION; default: the profile's region)")
	c.Long = `TARGET is [region/][user@]name (the instance's Name tag), an instance id (i-...), or an alias
from ~/.ssh/config written by 'wrangl dev ssh'. Without a TARGET you pick from a list.
All commands use your AWS credentials; the AWS CLI (and for connect/tunnel the Session Manager
plugin) must be installed.`
	return c
}

func argOr(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

func devList(o *devOpts) *cobra.Command {
	var state string
	c := &cobra.Command{
		Use: "list [REGION]", Short: "List EC2 instances (name, id, state, private IP)", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, _, err := o.resolver().Session(cmd.Context(), argOr(args, 0))
			if err != nil {
				return err
			}
			var states []string
			if state != "all" {
				states = strings.Split(state, ",")
			}
			l, err := s.List(cmd.Context(), states...)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tID\tSTATE\tPRIVATE IP\tREGION")
			for _, i := range l {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", dash(i.Name), i.ID, i.State, dash(i.PrivateIP), i.Region)
			}
			return tw.Flush()
		},
	}
	c.Flags().StringVar(&state, "state", "running,stopped", "states to list: comma list of pending,running,stopping,stopped, or all")
	return c
}

// execAWS replaces this process with the aws CLI (interactive sessions need the terminal).
func execAWS(args ...string) error {
	p, err := exec.LookPath("aws")
	if err != nil {
		return fmt.Errorf("aws CLI not found in PATH (install it and the Session Manager plugin): %w", err)
	}
	return syscall.Exec(p, append([]string{"aws"}, args...), os.Environ())
}

func sessionArgs(s *awsx.Session, i *awsx.Instance) []string {
	a := []string{"ssm", "start-session", "--target", i.ID, "--region", i.Region}
	if s.Profile != "" {
		a = append(a, "--profile", s.Profile)
	}
	return a
}

func devConnect(o *devOpts) *cobra.Command {
	return &cobra.Command{
		Use: "connect [TARGET]", Short: "Open an interactive SSM Session Manager shell", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, i, _, err := o.resolver().Resolve(cmd.Context(), argOr(args, 0))
			if err != nil {
				return err
			}
			return execAWS(sessionArgs(s, i)...)
		},
	}
}

func devTunnel(o *devOpts) *cobra.Command {
	var open bool
	c := &cobra.Command{
		Use: "tunnel [TARGET] RPORT[:LPORT]", Short: "Forward a local port to a port on the instance over SSM", Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := ssmtunnel.ParsePortSpec(args[len(args)-1])
			if err != nil {
				return err
			}
			s, i, _, err := o.resolver().Resolve(cmd.Context(), argOr(args[:len(args)-1], 0))
			if err != nil {
				return err
			}
			if spec.Local == 0 {
				if spec.Local, err = ssmtunnel.FreePort(); err != nil {
					return err
				}
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			url := fmt.Sprintf("http://localhost:%d", spec.Local)
			ready := func() {
				fmt.Fprintf(cmd.ErrOrStderr(), "Tunnel ready: %s -> %s:%d (Ctrl-C closes it)\n", url, i.Name, spec.Remote)
				if open {
					openBrowser(url)
				}
			}
			return ssmtunnel.Run(ctx, ssmtunnel.Args(i.ID, s.Profile, i.Region, spec.Remote, spec.Local), spec.Local, 30*time.Second, ready)
		},
	}
	c.Flags().BoolVar(&open, "open", false, "open the forwarded port in a browser when ready")
	return c
}

func openBrowser(url string) {
	for _, b := range []string{"open", "xdg-open"} {
		if p, err := exec.LookPath(b); err == nil {
			exec.Command(p, url).Start()
			return
		}
	}
}

func devStartStop(o *devOpts, start bool) *cobra.Command {
	use, short, states := "stop [TARGET]", "Stop a running instance", []string{"running"}
	if start {
		use, short, states = "start [TARGET]", "Start a stopped instance and wait until it is running", []string{"stopped"}
	}
	return &cobra.Command{
		Use: use, Short: short, Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, i, _, err := o.resolver().Resolve(cmd.Context(), argOr(args, 0), states...)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !start {
				if err := s.Stop(cmd.Context(), i.ID); err != nil {
					return err
				}
				fmt.Fprintf(out, "stopping %s (%s)\n", i.Name, i.ID)
				return nil
			}
			fmt.Fprintf(out, "starting %s (%s)...\n", i.Name, i.ID)
			if err := s.Start(cmd.Context(), i.ID); err != nil {
				return err
			}
			fmt.Fprintf(out, "%s is running\n", i.Name)
			return nil
		},
	}
}

// ---- ssh ------------------------------------------------------------------------------------

// sshPlan is what `dev ssh` and `dev ssh-config` resolved: who to log in as and which alias.
type sshPlan struct {
	s     *awsx.Session
	inst  *awsx.Instance
	user  string
	alias string
}

// planSSH resolves arg to an instance and a login user. A bare name that is a Host block written
// by wrangl dev reuses its region and profile, so no picker is needed.
func planSSH(ctx context.Context, o *devOpts, arg string) (*sshPlan, error) {
	r := o.resolver()
	t := awsx.ParseTarget(arg)
	user := t.User
	if t.Name != "" && t.Region == "" && t.ID == "" {
		if m := devbox.LookupHost(t.Name); m.Region != "" {
			if r.Region == "" {
				r.Region = m.Region
			}
			if r.Profile == "" {
				r.Profile = m.Profile
			}
			if user == "" {
				user = m.User
			}
		}
	}
	s, i, _, err := r.Resolve(ctx, arg)
	if err != nil {
		return nil, err
	}
	if user == "" {
		user = "ec2-user"
	}
	alias := i.Name
	if alias == "" {
		alias = i.ID
	}
	return &sshPlan{s: s, inst: i, user: user, alias: alias}, nil
}

func (p *sshPlan) block() devbox.HostBlock {
	exe, _ := os.Executable()
	return devbox.HostBlock{Name: p.alias, InstanceID: p.inst.ID, Region: p.inst.Region, Profile: p.s.Profile, User: p.user, Wrangl: exe}
}

// pushKey makes sure the dedicated key exists and registers it with the instance for 60 s.
func (p *sshPlan) pushKey(ctx context.Context) error {
	pub, err := devbox.EnsureKeyPair()
	if err != nil {
		return fmt.Errorf("SSH key: %w", err)
	}
	if err := eic.PushPublicKey(ctx, ec2instanceconnect.NewFromConfig(p.s.Cfg), p.inst.ID, p.user, pub); err != nil {
		return fmt.Errorf("push SSH public key: %w", err)
	}
	return nil
}

func prepareNetwork(ctx context.Context, p *sshPlan, w io.Writer) error {
	c := ec2.NewFromConfig(p.s.Cfg)
	if err := eic.EnsureEndpoint(ctx, c, p.inst.VPCID, p.inst.SubnetID, w); err != nil {
		return fmt.Errorf("EIC endpoint: %w", err)
	}
	if err := eic.EnsureSSHFromVPC(ctx, c, p.inst.VPCID, p.inst.SecurityGroupIDs); err != nil {
		return fmt.Errorf("security group: %w", err)
	}
	fmt.Fprintln(w, "EIC endpoint and the SSH-from-VPC rule are in place")
	return nil
}

func devSSH(o *devOpts) *cobra.Command {
	var prep bool
	c := &cobra.Command{
		Use: "ssh [TARGET] [-- SSH-ARGS or COMMAND]", Short: "SSH to an instance through EC2 Instance Connect (no open port 22, no key to manage)",
		Long: `Pushes a one-time public key (valid 60 s) with EC2 Instance Connect, writes a Host block to
~/.ssh/config and runs ssh. The Host block's ProxyCommand ('wrangl dev proxy') pushes a fresh key on
every connection, so a plain 'ssh NAME', scp or an editor's remote mode keep working afterwards.
The instance needs ec2-instance-connect installed (wrangl dev setup does) and, unless it has a
public address, an EC2 Instance Connect Endpoint in its VPC: --prepare-network creates it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, rest := "", args
			if d := cmd.ArgsLenAtDash(); d >= 0 {
				rest = args[d:]
				args = args[:d]
			}
			if len(args) > 1 {
				return errors.New("at most one TARGET before --")
			}
			target = argOr(args, 0)
			if cmd.ArgsLenAtDash() < 0 {
				rest = nil
			}
			p, err := planSSH(cmd.Context(), o, target)
			if err != nil {
				return err
			}
			if prep {
				if err := prepareNetwork(cmd.Context(), p, cmd.ErrOrStderr()); err != nil {
					return err
				}
			}
			if err := p.pushKey(cmd.Context()); err != nil {
				return err
			}
			if err := devbox.UpsertHostBlock(p.block()); err != nil {
				return fmt.Errorf("SSH config: %w", err)
			}
			sshPath, err := exec.LookPath("ssh")
			if err != nil {
				return fmt.Errorf("ssh not found in PATH: %w", err)
			}
			return syscall.Exec(sshPath, append([]string{"ssh", p.alias}, rest...), os.Environ())
		},
	}
	c.Flags().BoolVar(&prep, "prepare-network", false, "create the EIC endpoint and the SSH-from-VPC security group rule first")
	return c
}

func devSSHConfig(o *devOpts) *cobra.Command {
	return &cobra.Command{
		Use: "ssh-config [TARGET]", Short: "Write the ~/.ssh/config Host block for an instance without connecting", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := planSSH(cmd.Context(), o, argOr(args, 0))
			if err != nil {
				return err
			}
			if err := devbox.UpsertHostBlock(p.block()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "~/.ssh/config updated: ssh %s\n", p.alias)
			return nil
		},
	}
}

func devProxy(o *devOpts) *cobra.Command {
	var user string
	c := &cobra.Command{
		Use: "proxy INSTANCE-ID", Short: "ssh ProxyCommand: push a fresh EIC key, then open the tunnel", Args: cobra.ExactArgs(1), Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !awsx.IsInstanceID(args[0]) {
				return fmt.Errorf("%q is not an instance id", args[0])
			}
			s, i, _, err := o.resolver().Resolve(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := &sshPlan{s: s, inst: i, user: user}
			if err := p.pushKey(cmd.Context()); err != nil {
				return err
			}
			a := []string{"ec2-instance-connect", "open-tunnel", "--instance-id", i.ID, "--region", i.Region}
			if s.Profile != "" {
				a = append(a, "--profile", s.Profile)
			}
			return execAWS(a...)
		},
	}
	c.Flags().StringVar(&user, "user", "ec2-user", "login user the key is pushed for (ssh's %r)")
	return c
}

// ---- setup ----------------------------------------------------------------------------------

func devSetup(g *Global, o *devOpts) *cobra.Command {
	var tmpl, file, via, user, sshHost string
	var with []string
	var idle time.Duration
	var dry, revoke bool
	c := &cobra.Command{
		Use: "setup [TARGET]", Short: "Install a toolchain on an instance with mise (default: Go, Rust, Node, Python, Java, AWS CLI, CLI tools)",
		Long: `Applies a mise config to the machine, the same way a runner template is applied:
  1. the profile's setup_script runs as root when its content changed (the default one only
     installs git, tar, unzip and ec2-instance-connect),
  2. the mise config is written to ~/.config/mise/conf.d/wrangl-dev.toml of --user (your own
     config.toml is left alone),
  3. 'mise install' runs as that user.
It is idempotent: run it again after editing the profile. The profile is, in order of
precedence: --template NAME (a runner template from the controller), --file FILE (YAML with the
keys mise_toml and setup_script), or the built-in default; --with adds optional overlays.
Transport: SSM Run Command by default (nothing to open; output streams from CloudWatch Logs when
the instance role allows it), --via ssh for EC2 Instance Connect + ssh, or --ssh HOST for any
machine you can ssh to (it does not have to be on AWS).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			prof, err := loadProfile(ctx, g, tmpl, file, with)
			if err != nil {
				return err
			}
			if idle > 0 {
				prof.SetupScript = strings.TrimRight(prof.SetupScript, "\n") + "\n" + devbox.IdleStopScript(idle)
			}
			if user == "" {
				user = "ec2-user"
				if t := awsx.ParseTarget(argOr(args, 0)); t.User != "" {
					user = t.User
				}
			}
			script := runner.ApplyScript(prof.Apply(), user, runner.ScriptOptions{})
			if dry {
				fmt.Fprint(out, script)
				return nil
			}
			if sshHost != "" {
				return runOverSSH(ctx, sshHost, script, out, cmd.ErrOrStderr())
			}
			p, err := planSSH(ctx, o, argOr(args, 0))
			if err != nil {
				return err
			}
			p.user = user
			warnLegacyRules(ctx, p, revoke, cmd.ErrOrStderr())
			switch via {
			case "ssm":
				run := ssmrun.New(p.s.Cfg)
				if on, err := run.Online(ctx, p.inst.ID); err == nil && !on {
					return fmt.Errorf("the SSM agent on %s is not online (needs the AmazonSSMManagedInstanceCore role and outbound access); try --via ssh", p.inst.ID)
				}
				fmt.Fprintf(out, "applying the toolchain to %s (%s) as %s over SSM\n", p.inst.Name, p.inst.ID, user)
				return run.Run(ctx, p.inst.ID, script, out)
			case "ssh":
				if err := p.pushKey(ctx); err != nil {
					return err
				}
				if err := devbox.UpsertHostBlock(p.block()); err != nil {
					return err
				}
				return runOverSSH(ctx, p.alias, script, out, cmd.ErrOrStderr())
			}
			return fmt.Errorf("--via must be ssm or ssh, got %q", via)
		},
	}
	f := c.Flags()
	f.StringVar(&tmpl, "template", "", "use the mise_toml and setup_script of this runner template (needs the controller)")
	f.StringVar(&file, "file", "", "use a YAML profile file (keys mise_toml, setup_script)")
	f.StringSliceVar(&with, "with", nil, "add overlays: "+strings.Join(devbox.OverlayNames(), ", "))
	f.StringVar(&via, "via", "ssm", "transport for an AWS instance: ssm or ssh")
	f.StringVar(&sshHost, "ssh", "", "apply over plain ssh to HOST (any machine; no AWS involved)")
	f.StringVar(&user, "user", "", "user the tools are installed for (default: user@ of TARGET, else ec2-user)")
	f.DurationVar(&idle, "idle-stop", 0, "also power the instance off after this long idle (e.g. 4h)")
	f.BoolVar(&dry, "dry-run", false, "print the script that would run and exit")
	f.BoolVar(&revoke, "revoke-legacy-rules", false, "remove the all-ports 'ssm-managed' security group rules an earlier standalone tool created")
	return c
}

func loadProfile(ctx context.Context, g *Global, tmpl, file string, with []string) (devbox.Profile, error) {
	var p devbox.Profile
	switch {
	case tmpl != "" && file != "":
		return p, errors.New("use --template or --file, not both")
	case tmpl != "":
		c, err := apiClient(g)
		if err != nil {
			return p, err
		}
		t, err := c.Template(ctx, tmpl)
		if err != nil {
			return p, err
		}
		p = devbox.Profile{MiseTOML: t.MiseTOML, SetupScript: t.SetupScript}
	case file != "":
		var err error
		if p, err = devbox.LoadFile(file); err != nil {
			return p, err
		}
	default:
		p = devbox.Default()
	}
	return p.Merge(with...)
}

// runOverSSH feeds the script to `sudo bash -s` on host through the system ssh.
func runOverSSH(ctx context.Context, host, script string, out, errw io.Writer) error {
	cmd := exec.CommandContext(ctx, "ssh", host, "sudo bash -s")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(script), out, errw
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh %s: %w", host, err)
	}
	return nil
}

// warnLegacyRules reports (and with revoke, removes) the all-ports ingress rules an earlier
// tool added for the caller's IP; wrangl never creates them.
func warnLegacyRules(ctx context.Context, p *sshPlan, revoke bool, w io.Writer) {
	c := ec2.NewFromConfig(p.s.Cfg)
	rules, err := eic.FindLegacyRules(ctx, c, p.inst.SecurityGroupIDs)
	if err != nil || len(rules) == 0 {
		return
	}
	if revoke {
		if err := eic.RevokeLegacyRules(ctx, c, rules); err != nil {
			fmt.Fprintf(w, "could not revoke the 'ssm-managed' rules: %v\n", err)
			return
		}
		fmt.Fprintln(w, "revoked the 'ssm-managed' security group rules")
		return
	}
	for sg := range rules {
		fmt.Fprintf(w, "warning: security group %s has an all-ports 'ssm-managed' rule from an earlier standalone tool; remove it with --revoke-legacy-rules (wrangl dev uses SSM and EIC and needs no open ports)\n", sg)
	}
}

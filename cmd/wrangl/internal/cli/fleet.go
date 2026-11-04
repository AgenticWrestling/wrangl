package cli

import (
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/dbtime"

	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/config"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/provision"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

func metricsCols(m json.RawMessage) (cpu, mem, disk string) {
	var hb proto.Heartbeat
	if len(m) == 0 || json.Unmarshal(m, &hb) != nil {
		return "-", "-", "-"
	}
	f := func(v float64) string { return fmt.Sprintf("%.0f%%", v) }
	return f(hb.CPUPercent), f(hb.MemPercent), f(hb.DiskPercent)
}

// TeardownEC2 is overridable in tests so teardown can run against a fake.
var TeardownEC2 = func(ctx context.Context, region, profile string) (provision.EC2API, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	ac, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	if ac.Region == "" {
		return nil, errors.New("no AWS region: pass --region or set AWS_REGION")
	}
	return ec2.NewFromConfig(ac), nil
}

func fleetCmd(g *Global) *cobra.Command {
	var all bool
	list := &cobra.Command{
		Use: "list", Short: "List runners", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			rs, err := c.Runners(cmd.Context(), all)
			if err != nil {
				return err
			}
			now := time.Now()
			return emit(cmd, g, rs, func(w io.Writer) {
				fmt.Fprintln(w, "NAME\tSTATE\tTEMPLATE\tINSTANCE\tTYPE\tIP\tCPU\tMEM\tDISK\tHEARTBEAT\tIDLE")
				for _, r := range rs {
					cpu, mem, disk := metricsCols(r.Metrics)
					idle := "-"
					if r.State == "idle" {
						idle = dbtime.AgoShort(r.IdleSince, now)
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, stateLabel(r), r.Template,
						dash(r.InstanceID), dash(r.InstanceType), dash(r.PrivateIP), cpu, mem, disk, dbtime.AgoShort(r.LastHeartbeatAt, now), idle)
				}
			})
		},
	}
	list.Flags().BoolVar(&all, "all", false, "include terminated runners")

	show := &cobra.Command{
		Use: "show <runner>", Short: "Show a runner", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			r, err := c.Runner(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			now := time.Now()
			return emit(cmd, g, r, func(w io.Writer) {
				cpu, mem, disk := metricsCols(r.Metrics)
				origin := "launched by this controller (reaped after idle timeout)"
				if !r.Launched {
					origin = "adopted via enroll-token (never reaped)"
				}
				tmpl := fmt.Sprintf("%s (applied v%d of v%d)", r.Template, r.TemplateVersion, r.WantVersion)
				rows := [][2]string{
					{"name", r.Name}, {"state", stateLabel(r)}, {"template", tmpl}, {"origin", origin},
					{"instance", dash(r.InstanceID) + "  " + r.InstanceType + "  " + r.AZ}, {"private ip", dash(r.PrivateIP)},
					{"machine", fmt.Sprintf("%s, %d vCPU, %d MiB, %d GiB", dash(r.Arch), r.VCPU, r.RAMMiB, r.DiskGiB)},
					{"cpu / mem / disk", cpu + " / " + mem + " / " + disk},
					{"last heartbeat", dbtime.AgoShort(r.LastHeartbeatAt, now) + " ago"},
				}
				if r.State == "idle" && r.Launched {
					rows = append(rows, [2]string{"idle", fmt.Sprintf("%s of %s", dbtime.AgoShort(r.IdleSince, now), time.Duration(r.IdleTimeoutS)*time.Second)})
				}
				if r.BinarySHA != "" {
					bin := "sha256 " + shortSHA(r.BinarySHA)
					if r.UpgradeSHA != "" {
						bin += " → upgrading to " + shortSHA(r.UpgradeSHA)
					}
					rows = append(rows, [2]string{"wrangl binary", bin})
				}
				if r.LastError != "" {
					rows = append(rows, [2]string{"last error", r.LastError})
				}
				for _, kv := range rows {
					fmt.Fprintf(w, "%s\t%s\n", kv[0], kv[1])
				}
				if r.TemplateLog != "" {
					fmt.Fprintf(w, "template log\t\n%s\n", lastLines(r.TemplateLog, 15))
				}
			})
		},
	}

	var tmpl string
	var count int
	launch := &cobra.Command{
		Use: "launch", Short: "Launch runners on new EC2 instances", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			rs, err := c.Launch(cmd.Context(), tmpl, count)
			if err != nil {
				return err
			}
			return emit(cmd, g, rs, func(w io.Writer) {
				fmt.Fprintln(w, "NAME\tINSTANCE\tTYPE\tIP")
				for _, r := range rs {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Name, r.InstanceID, r.InstanceType, r.PrivateIP)
				}
			})
		},
	}
	launch.Flags().StringVar(&tmpl, "template", "", "runner template (required)")
	launch.Flags().IntVar(&count, "count", 1, "how many")
	launch.MarkFlagRequired("template")

	terminate := &cobra.Command{
		Use: "terminate <runner>", Short: "Terminate a controller-launched runner now", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			if err := c.Terminate(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "terminated %s\n", args[0])
			return nil
		},
	}

	var tokTmpl string
	var ttl time.Duration
	token := &cobra.Command{
		Use: "enroll-token", Short: "Mint a one-time enrolment token (adopt an existing box)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			t, err := c.EnrollToken(cmd.Context(), tokTmpl, ttl)
			if err != nil {
				return err
			}
			return emit(cmd, g, t, func(w io.Writer) {
				fmt.Fprintf(w, "token\t%s\nexpires\t%s\nrun on the box\t%s\n", t.Token, t.ExpiresAt.Format(time.RFC3339), t.Command)
			})
		},
	}
	token.Flags().StringVar(&tokTmpl, "template", "", "runner template (required)")
	token.Flags().DurationVar(&ttl, "ttl", 30*time.Minute, "token lifetime")
	token.MarkFlagRequired("template")

	var region, profile, ctlID string
	var allCtl, yes bool
	teardown := &cobra.Command{
		Use:   "teardown",
		Short: "Terminate every wrangl-tagged EC2 instance (talks to AWS directly; works without the controller)",
		Long: `Finds instances tagged wrangl:managed=true (and, unless --all-controllers, the
controller's wrangl:controller id) and terminates them after confirmation. Uses your local
AWS credentials, so it works even when the controller is gone. Untagged instances are never
touched, and ownership is re-checked on each instance before it is terminated.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if ctlID == "" && !allCtl {
				id, err := localControllerID(cmd.Context(), g)
				if err != nil {
					return fmt.Errorf("pass --controller-id or --all-controllers (could not read this host's controller id: %v)", err)
				}
				ctlID = id
			}
			api, err := TeardownEC2(ctx, region, profile)
			if err != nil {
				return err
			}
			p := &provision.Provisioner{EC2: api}
			insts, err := p.ListManaged(ctx, ctlID)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			scope := "controller " + ctlID
			if ctlID == "" {
				scope = "ALL wrangl controllers"
			}
			if len(insts) == 0 {
				fmt.Fprintf(out, "no wrangl instances found for %s\n", scope)
				return nil
			}
			fmt.Fprintf(out, "%d instance(s) tagged for %s:\n", len(insts), scope)
			for _, i := range insts {
				fmt.Fprintf(out, "  %s  %-10s %-12s %s  runner=%s\n", i.ID, i.State, i.Type, i.PrivateIP, i.Tags[provision.TagRunner])
			}
			if !yes {
				fmt.Fprint(out, "terminate them? type 'yes': ")
				line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if strings.TrimSpace(line) != "yes" {
					return errors.New("aborted")
				}
			}
			done, err := p.Teardown(ctx, ctlID)
			for _, i := range done {
				fmt.Fprintf(out, "terminated %s (%s)\n", i.ID, i.Tags[provision.TagRunner])
			}
			return err
		},
	}
	tf := teardown.Flags()
	tf.StringVar(&region, "region", "", "AWS region (default: AWS_REGION / profile)")
	tf.StringVar(&profile, "profile", "", "AWS shared-config profile")
	tf.StringVar(&ctlID, "controller-id", "", "only this controller's instances (default: read from this host)")
	tf.BoolVar(&allCtl, "all-controllers", false, "every wrangl-managed instance in the region")
	tf.BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	teardown.MarkFlagsMutuallyExclusive("controller-id", "all-controllers")

	return group("fleet", "Manage runners",
		list, show, launch,
		runnerAction(g, "cordon", "Stop assigning new work (the idle reaper still applies)"),
		runnerAction(g, "uncordon", "Resume assigning work"),
		runnerAction(g, "drain", "Stop new work; terminate once idle (controller-launched runners)"),
		terminate, token, teardown,
	)
}

// localControllerID reads meta.controller_id from this host's controller database.
func localControllerID(ctx context.Context, g *Global) (string, error) {
	path := config.Resolve(g.Config, "WRANGL_CONFIG", "", config.DefaultControllerConfig)
	c, err := config.LoadController(path)
	if err != nil {
		return "", err
	}
	d, err := db.Open(ctx, controller.DBPath(c.DataDir))
	if err != nil {
		return "", err
	}
	defer d.Close()
	return d.RQ.GetMeta(ctx, controller.MetaControllerID)
}

func runnerAction(g *Global, action, short string) *cobra.Command {
	return &cobra.Command{
		Use: action + " <runner>", Short: short, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient(g)
			if err != nil {
				return err
			}
			if err := c.RunnerAction(cmd.Context(), args[0], action); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", args[0], action)
			return nil
		},
	}
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func stateLabel(r proto.RunnerView) string {
	switch {
	case r.Draining:
		return r.State + " (draining)"
	case r.Cordoned:
		return r.State + " (cordoned)"
	case r.UpgradeSHA != "":
		return r.State + " (upgrading)"
	}
	return r.State
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

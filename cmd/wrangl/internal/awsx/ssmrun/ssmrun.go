// Package ssmrun runs a shell script on an instance with SSM Run Command and streams its
// output: from CloudWatch Logs while it runs (needs the /aws/ssm/AWS-RunShellScript log group
// and the instance role's logs permissions), else from the invocation record (the last
// 24,000 characters, shown when the command ends).
package ssmrun

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// LogGroup is the log group Run Command writes to for AWS-RunShellScript.
const LogGroup = "/aws/ssm/AWS-RunShellScript"

// SSMAPI is the SSM subset used.
type SSMAPI interface {
	SendCommand(context.Context, *ssm.SendCommandInput, ...func(*ssm.Options)) (*ssm.SendCommandOutput, error)
	GetCommandInvocation(context.Context, *ssm.GetCommandInvocationInput, ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error)
	DescribeInstanceInformation(context.Context, *ssm.DescribeInstanceInformationInput, ...func(*ssm.Options)) (*ssm.DescribeInstanceInformationOutput, error)
}

// LogsAPI is the CloudWatch Logs subset used.
type LogsAPI interface {
	GetLogEvents(context.Context, *cloudwatchlogs.GetLogEventsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error)
}

// Runner sends scripts and follows them.
type Runner struct {
	SSM      SSMAPI
	Logs     LogsAPI       // nil: no streaming, output only at the end
	Interval time.Duration // poll interval; default 2s
	Timeout  int32         // command timeout in seconds; default 3600
}

// New builds a Runner from an AWS config.
func New(cfg aws.Config) *Runner {
	return &Runner{SSM: ssm.NewFromConfig(cfg), Logs: cloudwatchlogs.NewFromConfig(cfg)}
}

// Online reports whether the SSM agent on the instance is connected.
func (r *Runner) Online(ctx context.Context, instanceID string) (bool, error) {
	out, err := r.SSM.DescribeInstanceInformation(ctx, &ssm.DescribeInstanceInformationInput{
		Filters: []ssmtypes.InstanceInformationStringFilter{{Key: aws.String("InstanceIds"), Values: []string{instanceID}}},
	})
	if err != nil {
		return false, err
	}
	for _, i := range out.InstanceInformationList {
		if i.PingStatus == ssmtypes.PingStatusOnline {
			return true, nil
		}
	}
	return false, nil
}

// StreamName is the CloudWatch stream of a command's shell plugin.
func StreamName(commandID, instanceID string) string {
	return fmt.Sprintf("%s/%s/aws-runshellscript/0.awsrunshellscriptplugin", commandID, instanceID)
}

func (r *Runner) interval() time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	return 2 * time.Second
}

// Run sends script (run as root by AWS-RunShellScript) and follows it to the end, writing its
// output to w. It returns an error if the command did not succeed.
func (r *Runner) Run(ctx context.Context, instanceID, script string, w io.Writer) error {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 3600
	}
	in := &ssm.SendCommandInput{
		DocumentName: aws.String("AWS-RunShellScript"), InstanceIds: []string{instanceID},
		Parameters: map[string][]string{"commands": {script}}, TimeoutSeconds: aws.Int32(timeout),
	}
	if r.Logs != nil {
		in.CloudWatchOutputConfig = &ssmtypes.CloudWatchOutputConfig{CloudWatchLogGroupName: aws.String(LogGroup), CloudWatchOutputEnabled: true}
	}
	out, err := r.SSM.SendCommand(ctx, in)
	if err != nil {
		return fmt.Errorf("send-command: %w", err)
	}
	id := aws.ToString(out.Command.CommandId)
	fmt.Fprintf(w, "SSM command %s sent; following its output (Ctrl-C detaches, the command keeps running)\n", id)
	return r.follow(ctx, id, instanceID, w)
}

type cursor struct {
	token *string
	last  int64
	n     int
}

func (r *Runner) pull(ctx context.Context, stream string, c *cursor, w io.Writer) {
	if r.Logs == nil {
		return
	}
	out, err := r.Logs.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
		LogGroupName: aws.String(LogGroup), LogStreamName: aws.String(stream), NextToken: c.token, StartFromHead: aws.Bool(true),
	})
	if err != nil { // the stream does not exist until the first output; retry
		return
	}
	for _, e := range out.Events {
		if aws.ToInt64(e.Timestamp) >= c.last {
			fmt.Fprintln(w, aws.ToString(e.Message))
			c.last = aws.ToInt64(e.Timestamp)
			c.n++
		}
	}
	c.token = out.NextForwardToken
}

func (r *Runner) follow(ctx context.Context, cmdID, instanceID string, w io.Writer) error {
	stream := StreamName(cmdID, instanceID)
	var c cursor
	for {
		select {
		case <-time.After(r.interval()):
		case <-ctx.Done():
			return ctx.Err()
		}
		r.pull(ctx, stream, &c, w)
		inv, err := r.SSM.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{CommandId: aws.String(cmdID), InstanceId: aws.String(instanceID)})
		if err != nil {
			continue // the invocation can take a moment to exist
		}
		switch inv.Status {
		case ssmtypes.CommandInvocationStatusSuccess, ssmtypes.CommandInvocationStatusFailed,
			ssmtypes.CommandInvocationStatusTimedOut, ssmtypes.CommandInvocationStatusCancelled:
		default:
			continue
		}
		// CloudWatch ingestion lags the status by a few seconds: drain until quiet.
		for empty, i := 0, 0; r.Logs != nil && empty < 2 && i < 6; i++ {
			select {
			case <-time.After(r.interval()):
			case <-ctx.Done():
				return ctx.Err()
			}
			before := c.n
			r.pull(ctx, stream, &c, w)
			if c.n == before {
				empty++
			} else {
				empty = 0
			}
		}
		if c.n == 0 { // nothing streamed: show the invocation's own (truncated) output
			fmt.Fprint(w, aws.ToString(inv.StandardOutputContent))
			fmt.Fprint(w, aws.ToString(inv.StandardErrorContent))
		}
		if inv.Status == ssmtypes.CommandInvocationStatusSuccess {
			return nil
		}
		return fmt.Errorf("command %s (exit code %d)", inv.Status, inv.ResponseCode)
	}
}

// cmd/ssm/internal/cwlogs/cwlogs.go
package cwlogs

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	ssmaws "github.com/nreid/devhost/cmd/ssm/internal/aws"
)

const logGroup = "/aws/ssm/AWS-RunShellScript"

// drainLogs polls CloudWatch for up to 10 seconds after the command finishes,
// stopping early after two consecutive polls that yield no new events.
// CW ingestion lags behind SSM status by several seconds.
func drainLogs(ctx context.Context, cwClient *cloudwatchlogs.Client, streamName string, nextToken *string, lastTimestamp int64, w io.Writer) {
	deadline := time.Now().Add(10 * time.Second)
	emptyPolls := 0
	for time.Now().Before(deadline) {
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
		out, err := cwClient.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
			LogGroupName:  aws.String(logGroup),
			LogStreamName: aws.String(streamName),
			NextToken:     nextToken,
			StartFromHead: aws.Bool(true),
		})
		if err != nil {
			// Stream may not exist yet; keep retrying until deadline.
			continue
		}
		printed := 0
		for _, e := range out.Events {
			if aws.ToInt64(e.Timestamp) > lastTimestamp {
				fmt.Fprintln(w, aws.ToString(e.Message))
				lastTimestamp = aws.ToInt64(e.Timestamp)
				printed++
			}
		}
		nextToken = out.NextForwardToken
		if printed == 0 {
			emptyPolls++
			if emptyPolls >= 2 {
				return
			}
		} else {
			emptyPolls = 0
		}
	}
}

func LogStreamName(commandID, instanceID string) string {
	return fmt.Sprintf("%s/%s/aws-runshellscript/0.awsrunshellscriptplugin", commandID, instanceID)
}

func Tail(ctx context.Context, profile, region, commandID, instanceID string, w io.Writer) error {
	cfg, err := ssmaws.LoadConfig(ctx, profile, region)
	if err != nil {
		return err
	}
	cwClient := cloudwatchlogs.NewFromConfig(cfg)
	ssmClient := ssm.NewFromConfig(cfg)

	streamName := LogStreamName(commandID, instanceID)
	var nextToken *string
	var lastTimestamp int64

	for {
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}

		// Fetch new log events.
		out, err := cwClient.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
			LogGroupName:  aws.String(logGroup),
			LogStreamName: aws.String(streamName),
			NextToken:     nextToken,
			StartFromHead: aws.Bool(true),
		})
		if err != nil {
			// transient errors are retried; propagate only on context cancellation
			if ctx.Err() != nil {
				return ctx.Err()
			}
		} else {
			for _, e := range out.Events {
				if aws.ToInt64(e.Timestamp) > lastTimestamp {
					fmt.Fprintln(w, aws.ToString(e.Message))
					lastTimestamp = aws.ToInt64(e.Timestamp)
				}
			}
			nextToken = out.NextForwardToken
		}

		// Check command status.
		inv, err := ssmClient.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{
			CommandId:  aws.String(commandID),
			InstanceId: aws.String(instanceID),
		})
		if err != nil {
			continue
		}

		switch inv.Status {
		case ssmtypes.CommandInvocationStatusSuccess:
			drainLogs(ctx, cwClient, streamName, nextToken, lastTimestamp, w)
			fmt.Fprintln(w, "✓ Provisioning complete")
			return nil
		case ssmtypes.CommandInvocationStatusFailed,
			ssmtypes.CommandInvocationStatusTimedOut,
			ssmtypes.CommandInvocationStatusCancelled:
			drainLogs(ctx, cwClient, streamName, nextToken, lastTimestamp, w)
			// Fall back to inline output from the invocation response when no CW logs arrived.
			if stdout := aws.ToString(inv.StandardOutputContent); stdout != "" {
				fmt.Fprint(w, stdout)
			}
			if stderr := aws.ToString(inv.StandardErrorContent); stderr != "" {
				fmt.Fprint(w, stderr)
			}
			return fmt.Errorf("provisioning %s (exit code %d)", inv.Status, inv.ResponseCode)
		}
	}
}

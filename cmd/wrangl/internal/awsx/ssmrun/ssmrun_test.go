package ssmrun

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type fakeSSM struct {
	sent     *ssm.SendCommandInput
	statuses []ssmtypes.CommandInvocationStatus
	stdout   string
	online   bool
}

func (f *fakeSSM) SendCommand(_ context.Context, in *ssm.SendCommandInput, _ ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	f.sent = in
	return &ssm.SendCommandOutput{Command: &ssmtypes.Command{CommandId: aws.String("cmd-1")}}, nil
}
func (f *fakeSSM) GetCommandInvocation(context.Context, *ssm.GetCommandInvocationInput, ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	s := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}
	return &ssm.GetCommandInvocationOutput{Status: s, StandardOutputContent: aws.String(f.stdout), ResponseCode: 3}, nil
}
func (f *fakeSSM) DescribeInstanceInformation(context.Context, *ssm.DescribeInstanceInformationInput, ...func(*ssm.Options)) (*ssm.DescribeInstanceInformationOutput, error) {
	if !f.online {
		return &ssm.DescribeInstanceInformationOutput{}, nil
	}
	return &ssm.DescribeInstanceInformationOutput{InstanceInformationList: []ssmtypes.InstanceInformation{{PingStatus: ssmtypes.PingStatusOnline}}}, nil
}

type fakeLogs struct {
	pages [][]string
	i     int
}

func (f *fakeLogs) GetLogEvents(context.Context, *cloudwatchlogs.GetLogEventsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	var ev []cwtypes.OutputLogEvent
	if f.i < len(f.pages) {
		for _, m := range f.pages[f.i] {
			ev = append(ev, cwtypes.OutputLogEvent{Message: aws.String(m), Timestamp: aws.Int64(int64(f.i + 1))})
		}
		f.i++
	}
	return &cloudwatchlogs.GetLogEventsOutput{Events: ev, NextForwardToken: aws.String("t")}, nil
}

func TestRunStreamsFromCloudWatch(t *testing.T) {
	s := &fakeSSM{statuses: []ssmtypes.CommandInvocationStatus{ssmtypes.CommandInvocationStatusInProgress, ssmtypes.CommandInvocationStatusSuccess}}
	r := &Runner{SSM: s, Logs: &fakeLogs{pages: [][]string{{"==> one"}, {"==> two"}}}, Interval: time.Millisecond}
	var b bytes.Buffer
	if err := r.Run(context.Background(), "i-1", "echo hi", &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "==> one") || !strings.Contains(b.String(), "==> two") {
		t.Fatalf("output: %q", b.String())
	}
	if s.sent.Parameters["commands"][0] != "echo hi" || s.sent.CloudWatchOutputConfig == nil || aws.ToInt32(s.sent.TimeoutSeconds) != 3600 {
		t.Fatalf("%+v", s.sent)
	}
}

func TestRunFallsBackToInvocationOutputAndReportsFailure(t *testing.T) {
	s := &fakeSSM{statuses: []ssmtypes.CommandInvocationStatus{ssmtypes.CommandInvocationStatusFailed}, stdout: "boom\n"}
	r := &Runner{SSM: s, Interval: time.Millisecond}
	var b bytes.Buffer
	err := r.Run(context.Background(), "i-1", "false", &b)
	if err == nil || !strings.Contains(err.Error(), "Failed") || !strings.Contains(err.Error(), "3") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(b.String(), "boom") || s.sent.CloudWatchOutputConfig != nil {
		t.Fatalf("output %q cw=%v", b.String(), s.sent.CloudWatchOutputConfig)
	}
}

func TestOnline(t *testing.T) {
	r := &Runner{SSM: &fakeSSM{online: true}}
	if ok, err := r.Online(context.Background(), "i-1"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	r = &Runner{SSM: &fakeSSM{}}
	if ok, _ := r.Online(context.Background(), "i-1"); ok {
		t.Fatal("want offline")
	}
}

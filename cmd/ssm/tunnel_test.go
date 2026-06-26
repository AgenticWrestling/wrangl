package main

import (
	"testing"
)

func TestBuildTunnelArgs(t *testing.T) {
	args := buildTunnelArgs("i-0abc123", "us-west-2", "myprofile", 3000, 8080)
	want := []string{
		"aws", "ssm", "start-session",
		"--target", "i-0abc123",
		"--region", "us-west-2",
		"--profile", "myprofile",
		"--document-name", "AWS-StartPortForwardingSession",
		"--parameters", `{"portNumber":["3000"],"localPortNumber":["8080"]}`,
	}
	if len(args) != len(want) {
		t.Fatalf("got %v\nwant %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("arg[%d]: got %q, want %q", i, args[i], want[i])
		}
	}
}

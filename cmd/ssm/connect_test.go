package main

import (
	"testing"
)

func TestBuildConnectArgs(t *testing.T) {
	args := buildConnectArgs("i-0abc123", "us-west-2", "myprofile")
	want := []string{
		"aws", "ssm", "start-session",
		"--target", "i-0abc123",
		"--region", "us-west-2",
		"--profile", "myprofile",
	}
	if len(args) != len(want) {
		t.Fatalf("got %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("arg[%d]: got %q, want %q", i, args[i], want[i])
		}
	}
}

package ssmtunnel

import (
	"context"
	"net"
	"slices"
	"testing"
	"time"
)

func TestParsePortSpec(t *testing.T) {
	for in, want := range map[string]PortSpec{"3000": {3000, 3000}, "3000:8080": {3000, 8080}} {
		if got, err := ParsePortSpec(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "x", "0", "70000", "80:x", "80:-1"} {
		if _, err := ParsePortSpec(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestArgs(t *testing.T) {
	a := Args("i-1", "dev", "us-west-2", 3000, 8080)
	if !slices.Contains(a, "portNumber=3000,localPortNumber=8080") || !slices.Contains(a, "--profile") || !slices.Contains(a, "us-west-2") {
		t.Fatal(a)
	}
	if b := Args("i-1", "", "", 1, 1); slices.Contains(b, "--profile") || slices.Contains(b, "--region") {
		t.Fatal(b)
	}
}

func TestWaitReady(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	if err := WaitReady(context.Background(), "127.0.0.1", l.Addr().(*net.TCPAddr).Port, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	p, _ := FreePort()
	if err := WaitReady(context.Background(), "127.0.0.1", p, 600*time.Millisecond); err == nil {
		t.Fatal("nothing listens; want error")
	}
}

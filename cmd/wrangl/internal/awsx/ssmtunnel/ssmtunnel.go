// Package ssmtunnel runs `aws ssm start-session` port-forward sessions: supervised (restarted
// with backoff when the session drops) for the runner's remote targets, one-shot for
// `wrangl dev tunnel`.
package ssmtunnel

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// PortSpec is "3000" (local = remote) or "3000:8080" (remote:local).
type PortSpec struct{ Remote, Local int }

// ParsePortSpec parses a port spec.
func ParsePortSpec(s string) (PortSpec, error) {
	if s == "" {
		return PortSpec{}, fmt.Errorf("port spec must not be empty")
	}
	a, b, two := strings.Cut(s, ":")
	r, err := strconv.Atoi(a)
	if err != nil || r < 1 || r > 65535 {
		return PortSpec{}, fmt.Errorf("invalid remote port %q", a)
	}
	if !two {
		return PortSpec{r, r}, nil
	}
	l, err := strconv.Atoi(b)
	if err != nil || l < 0 || l > 65535 {
		return PortSpec{}, fmt.Errorf("invalid local port %q", b)
	}
	return PortSpec{r, l}, nil
}

// Args builds the `aws ssm start-session` arguments (after "aws") for a port forward.
// Empty profile/region are left to the AWS CLI's own defaults.
func Args(instanceID, profile, region string, remote, local int) []string {
	a := []string{"ssm", "start-session", "--target", instanceID,
		"--document-name", "AWS-StartPortForwardingSession",
		"--parameters", fmt.Sprintf("portNumber=%d,localPortNumber=%d", remote, local)}
	if profile != "" {
		a = append(a, "--profile", profile)
	}
	if region != "" {
		a = append(a, "--region", region)
	}
	return a
}

// FreePort returns a loopback port nothing listens on right now.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// WaitReady dials host:port until it answers or the timeout passes.
func WaitReady(ctx context.Context, host string, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 2*time.Second)
		if err == nil {
			c.Close()
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Supervise runs the port-forward session until ctx is cancelled, restarting it with backoff
// (1s doubling to 1m, reset after a session that lasted a minute). It returns when stopped.
func Supervise(ctx context.Context, args []string, local int) {
	backoff := time.Second
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, "aws", args...)
		start := time.Now()
		cmd.Run()
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// Run runs one foreground session (output to the terminal) and returns once it exits or ctx is
// cancelled. ready, if non-nil, is called when the local port answers.
func Run(ctx context.Context, args []string, local int, timeout time.Duration, ready func()) error {
	cmd := exec.CommandContext(ctx, "aws", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting tunnel (is the aws CLI and the Session Manager plugin installed?): %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		if WaitReady(rctx, "127.0.0.1", local, timeout) == nil && ready != nil {
			ready()
		}
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		<-done
		return nil
	}
}

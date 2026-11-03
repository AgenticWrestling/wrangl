# SSM Tool — M3: `ssm tunnel` Subcommand

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement `ssm tunnel [region/name] <rport>[:<lport>] [--open]` — SSM port-forwarding session with optional browser launch on tunnel ready.

**Architecture:** Resolves instance, parses port spec, starts `aws ssm start-session --document-name AWS-StartPortForwardingSession` as a child process (not exec-replace, since we need to poll and clean up), polls for local port readiness, optionally opens browser, then blocks until Ctrl-C.

**Tech Stack:** Go stdlib `os/exec`, `net`, `os/signal`, M1 shared infrastructure

## Global Constraints

- Module path: `github.com/agenticwrestling/wrangl`
- Binary output: `~/bin/ssm`
- CGO_ENABLED=0
- M1 must be complete before starting M3

---

### Task 1: Port spec parser

**Files:**
- Create: `cmd/ssm/internal/tunnel/ports.go`
- Create: `cmd/ssm/internal/tunnel/ports_test.go`

**Interfaces:**
- Produces:
  ```go
  type PortSpec struct {
      RemotePort int
      LocalPort  int
  }
  // ParsePortSpec parses "3000" or "3000:8080" (remote:local).
  func ParsePortSpec(s string) (PortSpec, error)
  ```

- [ ] **Step 1: Write the failing tests**

```go
// cmd/ssm/internal/tunnel/ports_test.go
package tunnel_test

import (
	"testing"
	"github.com/agenticwrestling/wrangl/cmd/ssm/internal/tunnel"
)

func TestParsePortSpec(t *testing.T) {
	cases := []struct {
		input      string
		wantRemote int
		wantLocal  int
		wantErr    bool
	}{
		{"3000", 3000, 3000, false},
		{"3000:8080", 3000, 8080, false},
		{"abc", 0, 0, true},
		{"3000:abc", 0, 0, true},
		{"", 0, 0, true},
	}
	for _, c := range cases {
		spec, err := tunnel.ParsePortSpec(c.input)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParsePortSpec(%q): expected error, got nil", c.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsePortSpec(%q): unexpected error: %v", c.input, err)
			continue
		}
		if spec.RemotePort != c.wantRemote || spec.LocalPort != c.wantLocal {
			t.Errorf("ParsePortSpec(%q): got remote=%d local=%d, want remote=%d local=%d",
				c.input, spec.RemotePort, spec.LocalPort, c.wantRemote, c.wantLocal)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd ~/code/devhost
go test ./cmd/ssm/internal/tunnel/... -v
```

Expected: compile error — package does not exist.

- [ ] **Step 3: Create directory and implement**

```bash
mkdir -p ~/code/devhost/cmd/ssm/internal/tunnel
```

```go
// cmd/ssm/internal/tunnel/ports.go
package tunnel

import (
	"fmt"
	"strconv"
	"strings"
)

type PortSpec struct {
	RemotePort int
	LocalPort  int
}

func ParsePortSpec(s string) (PortSpec, error) {
	if s == "" {
		return PortSpec{}, fmt.Errorf("port spec must not be empty")
	}
	parts := strings.SplitN(s, ":", 2)
	remote, err := strconv.Atoi(parts[0])
	if err != nil {
		return PortSpec{}, fmt.Errorf("invalid remote port %q: %w", parts[0], err)
	}
	if len(parts) == 1 {
		return PortSpec{RemotePort: remote, LocalPort: remote}, nil
	}
	local, err := strconv.Atoi(parts[1])
	if err != nil {
		return PortSpec{}, fmt.Errorf("invalid local port %q: %w", parts[1], err)
	}
	return PortSpec{RemotePort: remote, LocalPort: local}, nil
}
```

- [ ] **Step 4: Run tests**

```bash
cd ~/code/devhost
go test ./cmd/ssm/internal/tunnel/... -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
cd ~/code/devhost
git add cmd/ssm/internal/tunnel/
git commit -m "feat: implement port spec parser for ssm tunnel"
```

---

### Task 2: Implement `ssm tunnel` subcommand

**Files:**
- Modify: `cmd/ssm/main.go` — replace `tunnelCmd()` stub

**Interfaces:**
- Consumes:
  - `resolveTarget(args, region) (string, *aws.Instance, error)` from M1
  - `tunnel.ParsePortSpec(s) (PortSpec, error)` from Task 1

- [ ] **Step 1: Write test for tunnel args construction**

```go
// cmd/ssm/tunnel_test.go
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
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd ~/code/devhost
go test ./cmd/ssm/... -run TestBuildTunnelArgs -v
```

Expected: compile error — `buildTunnelArgs` not defined.

- [ ] **Step 3: Replace tunnelCmd stub in main.go**

Add import `"github.com/agenticwrestling/wrangl/cmd/ssm/internal/tunnel"`.

Replace `tunnelCmd()`:

```go
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

			profile, inst, err := resolveTarget(targetArgs, "")
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
```

Add to imports: `"net"`, `"os/signal"`, `"time"`, `"fmt"`.

- [ ] **Step 4: Run tests**

```bash
cd ~/code/devhost
go test ./cmd/ssm/... -run TestBuildTunnelArgs -v
```

Expected: PASS

- [ ] **Step 5: Build**

```bash
cd ~/code/devhost
CGO_ENABLED=0 go build -o ~/bin/ssm ./cmd/ssm
~/bin/ssm tunnel --help
```

Expected: help text includes `--open` flag.

- [ ] **Step 6: Commit**

```bash
cd ~/code/devhost
git add cmd/ssm/main.go cmd/ssm/tunnel_test.go
git commit -m "feat: implement ssm tunnel subcommand with --open flag"
```

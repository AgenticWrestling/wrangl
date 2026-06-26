# SSM Tool — M2: `ssm connect` Subcommand

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement `ssm connect [region/name]` — prompts for profile, resolves the instance, and opens an interactive SSM Session Manager terminal.

**Architecture:** The connect subcommand resolves the target instance via `resolveTarget`, then exec-replaces the current process with `aws ssm start-session`. The AWS CLI session-manager-plugin handles the websocket; we cannot replicate this in the SDK.

**Tech Stack:** Go stdlib `os/exec`, M1 shared infrastructure (profiles, resolver, picker)

## Global Constraints

- Module path: `github.com/nreid/devhost`
- Binary output: `~/bin/ssm`
- CGO_ENABLED=0
- M1 must be complete before starting M2

---

### Task 1: Implement `ssm connect`

**Files:**
- Modify: `cmd/ssm/main.go` — replace `connectCmd()` stub

**Interfaces:**
- Consumes: `resolveTarget(args []string, defaultRegion string) (string, *aws.Instance, error)` from M1

- [ ] **Step 1: Write a test for the exec arguments construction**

```go
// cmd/ssm/connect_test.go
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
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd ~/code/devhost
go test ./cmd/ssm/... -run TestBuildConnectArgs -v
```

Expected: compile error — `buildConnectArgs` not defined.

- [ ] **Step 3: Replace connectCmd stub in main.go**

Replace the `connectCmd()` function:

```go
func connectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "connect [region/name]",
		Short: "Open interactive SSM terminal session",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profile, inst, err := resolveTarget(args, "")
			if err != nil {
				return err
			}
			execArgs := buildConnectArgs(inst.ID, inst.Region, profile)
			awsPath, err := exec.LookPath("aws")
			if err != nil {
				return fmt.Errorf("aws CLI not found in PATH: %w", err)
			}
			return syscall.Exec(awsPath, execArgs, os.Environ())
		},
	}
}

func buildConnectArgs(instanceID, region, profile string) []string {
	return []string{
		"aws", "ssm", "start-session",
		"--target", instanceID,
		"--region", region,
		"--profile", profile,
	}
}
```

Add to imports in main.go:
```go
"os/exec"
"syscall"
```

- [ ] **Step 4: Run tests**

```bash
cd ~/code/devhost
go test ./cmd/ssm/... -run TestBuildConnectArgs -v
```

Expected: PASS

- [ ] **Step 5: Build and smoke-test**

```bash
cd ~/code/devhost
CGO_ENABLED=0 go build -o ~/bin/ssm ./cmd/ssm
~/bin/ssm connect --help
```

Expected: help text printed, no errors.

- [ ] **Step 6: Commit**

```bash
cd ~/code/devhost
git add cmd/ssm/main.go cmd/ssm/connect_test.go
git commit -m "feat: implement ssm connect subcommand"
```

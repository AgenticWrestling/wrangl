# SSM Tool — M5: `ssm setup` Phase 2+3 — Remote Provisioning & CloudWatch Tail

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete `ssm setup` by sending the provisioning shell script via SSM `send-command` and tailing the CloudWatch Logs output until the command completes.

**Architecture:** Two internal packages: `provision` (builds and sends the shell script, polls command status) and `cwlogs` (tails log events). Both are called from `setupCmd()` after M4's Phase 1 completes.

**Tech Stack:** AWS SDK v2 (SSM, CloudWatch Logs), M1 shared infrastructure, M4 `setupCmd` base

## Global Constraints

- Module path: `github.com/agenticwrestling/wrangl`
- CGO_ENABLED=0
- M1 and M4 must be complete before starting M5
- Login user on instances: `ec2-user`
- SSM document used: `AWS-RunShellScript`
- CloudWatch log group: `/aws/ssm/AWS-RunShellScript`

---

### Task 1: CloudWatch Logs tail helper

**Files:**
- Create: `cmd/ssm/internal/cwlogs/cwlogs.go`
- Create: `cmd/ssm/internal/cwlogs/cwlogs_test.go`

**Interfaces:**
- Produces:
  ```go
  // Tail polls FilterLogEvents for a specific command/instance log stream,
  // writing new events to w until the SSM command reaches a terminal state.
  // commandID and instanceID are used to construct the log stream name.
  func Tail(ctx context.Context, profile, region, commandID, instanceID string, w io.Writer) error
  ```

- [ ] **Step 1: Add CloudWatch Logs dependency**

```bash
cd ~/code/devhost
go get github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs@latest
go get github.com/aws/aws-sdk-go-v2/service/ssm@latest
```

- [ ] **Step 2: Write failing test for stream name construction**

```go
// cmd/ssm/internal/cwlogs/cwlogs_test.go
package cwlogs_test

import (
	"testing"
	"github.com/agenticwrestling/wrangl/cmd/ssm/internal/cwlogs"
)

func TestLogStreamName(t *testing.T) {
	got := cwlogs.LogStreamName("cmd-123", "i-abc")
	want := "cmd-123/i-abc/aws-runshellscript/0.awsrunshellscriptplugin"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
```

- [ ] **Step 3: Run to verify it fails**

```bash
cd ~/code/devhost
go test ./cmd/ssm/internal/cwlogs/... -v
```

Expected: compile error — package does not exist.

- [ ] **Step 4: Create directory and implement cwlogs.go**

```bash
mkdir -p ~/code/devhost/cmd/ssm/internal/cwlogs
```

```go
// cmd/ssm/internal/cwlogs/cwlogs.go
package cwlogs

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

const logGroup = "/aws/ssm/AWS-RunShellScript"

func LogStreamName(commandID, instanceID string) string {
	return fmt.Sprintf("%s/%s/aws-runshellscript/0.awsrunshellscriptplugin", commandID, instanceID)
}

func Tail(ctx context.Context, profile, region, commandID, instanceID string, w io.Writer) error {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithSharedConfigProfile(profile),
		config.WithRegion(region),
	)
	if err != nil {
		return err
	}
	cwClient := cloudwatchlogs.NewFromConfig(cfg)
	ssmClient := ssm.NewFromConfig(cfg)

	streamName := LogStreamName(commandID, instanceID)
	var nextToken *string
	var lastTimestamp int64

	for {
		time.Sleep(2 * time.Second)

		// Fetch new log events.
		out, err := cwClient.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
			LogGroupName:  aws.String(logGroup),
			LogStreamName: aws.String(streamName),
			NextToken:     nextToken,
			StartFromHead: aws.Bool(true),
		})
		if err == nil {
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
			fmt.Fprintln(w, "✓ Provisioning complete")
			return nil
		case ssmtypes.CommandInvocationStatusFailed,
			ssmtypes.CommandInvocationStatusTimedOut,
			ssmtypes.CommandInvocationStatusCancelled:
			return fmt.Errorf("provisioning %s (exit code %d)", inv.Status, aws.ToInt32(inv.ResponseCode))
		}
	}
}
```

- [ ] **Step 5: Run tests**

```bash
cd ~/code/devhost
go test ./cmd/ssm/internal/cwlogs/... -run TestLogStreamName -v
```

Expected: PASS

- [ ] **Step 6: Commit**

```bash
cd ~/code/devhost
git add cmd/ssm/internal/cwlogs/
git commit -m "feat: CloudWatch Logs tail helper for SSM command output"
```

---

### Task 2: Remote provisioning script sender

**Files:**
- Create: `cmd/ssm/internal/provision/provision.go`

**Interfaces:**
- Produces:
  ```go
  // Run sends the dev toolchain install script to the instance via SSM send-command.
  // Returns the SSM Command ID.
  func Run(ctx context.Context, profile, region, instanceID string) (commandID string, err error)
  ```

- [ ] **Step 1: Create directory and implement provision.go**

```bash
mkdir -p ~/code/devhost/cmd/ssm/internal/provision
```

```go
// cmd/ssm/internal/provision/provision.go
package provision

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

const script = `#!/bin/bash
set -euo pipefail

echo "==> Installing ec2-instance-connect"
dnf install -y ec2-instance-connect

echo "==> Installing fish shell"
dnf install -y fish
chsh -s /usr/bin/fish ec2-user

echo "==> Installing Go"
dnf install -y golang

echo "==> Installing Rust"
sudo -u ec2-user sh -c 'curl --proto =https --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --no-modify-path'

echo "==> Installing Zed remote server"
ARCH=$(uname -m)
RELEASE=$(curl -sf https://api.github.com/repos/zed-industries/zed/releases/latest | grep tag_name | cut -d'"' -f4)
curl -fsSL "https://github.com/zed-industries/zed/releases/download/${RELEASE}/zed-remote-server-linux-${ARCH}.gz" \
  | gunzip > /usr/local/bin/zed-remote
chmod +x /usr/local/bin/zed-remote
echo "Installed Zed remote server ${RELEASE} (${ARCH})"

echo "==> Installing Headless Chrome"
cat > /etc/yum.repos.d/google-chrome.repo <<'REPO'
[google-chrome]
name=google-chrome
baseurl=http://dl.google.com/linux/chrome/rpm/stable/x86_64
enabled=1
gpgcheck=1
gpgkey=https://dl.google.com/linux/linux_signing_key.pub
REPO
dnf install -y google-chrome-stable

echo "==> All done"
`

func Run(ctx context.Context, profile, region, instanceID string) (string, error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithSharedConfigProfile(profile),
		config.WithRegion(region),
	)
	if err != nil {
		return "", err
	}
	client := ssm.NewFromConfig(cfg)

	out, err := client.SendCommand(ctx, &ssm.SendCommandInput{
		DocumentName: aws.String("AWS-RunShellScript"),
		InstanceIds:  []string{instanceID},
		Parameters: map[string][]string{
			"commands": {script},
		},
		TimeoutSeconds: aws.Int32(3600),
		CloudWatchOutputConfig: &ssm.CloudWatchOutputConfig{ // Note: wrong type, see below
		},
	})
	if err != nil {
		return "", fmt.Errorf("send-command: %w", err)
	}
	return aws.ToString(out.Command.CommandId), nil
}
```

> **Note:** The `CloudWatchOutputConfig` field uses `ssmtypes.CloudWatchOutputConfig`. Fix the import:

```go
// Replace the CloudWatchOutputConfig block with:
import (
	...
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// In SendCommandInput:
CloudWatchOutputConfig: &ssmtypes.CloudWatchOutputConfig{
    CloudWatchLogGroupName:  aws.String("/aws/ssm/AWS-RunShellScript"),
    CloudWatchOutputEnabled: true,
},
```

Full corrected provision.go:

```go
// cmd/ssm/internal/provision/provision.go
package provision

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

const script = `#!/bin/bash
set -euo pipefail

echo "==> Installing AWS CLI v2"
curl -fsSL "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o /tmp/awscliv2.zip
unzip -q /tmp/awscliv2.zip -d /tmp
/tmp/aws/install
rm -rf /tmp/aws /tmp/awscliv2.zip

echo "==> Installing ec2-instance-connect"
dnf install -y ec2-instance-connect

echo "==> Installing fish shell"
dnf install -y fish
chsh -s /usr/bin/fish ec2-user

echo "==> Installing Go"
dnf install -y golang

echo "==> Installing Rust"
sudo -u ec2-user sh -c 'curl --proto =https --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --no-modify-path'

echo "==> Installing Zed remote server"
ARCH=$(uname -m)
RELEASE=$(curl -sf https://api.github.com/repos/zed-industries/zed/releases/latest | grep tag_name | cut -d'"' -f4)
curl -fsSL "https://github.com/zed-industries/zed/releases/download/${RELEASE}/zed-remote-server-linux-${ARCH}.gz" \
  | gunzip > /usr/local/bin/zed-remote
chmod +x /usr/local/bin/zed-remote
echo "Installed Zed remote server ${RELEASE} (${ARCH})"

echo "==> Installing Headless Chrome"
cat > /etc/yum.repos.d/google-chrome.repo <<'REPO'
[google-chrome]
name=google-chrome
baseurl=http://dl.google.com/linux/chrome/rpm/stable/x86_64
enabled=1
gpgcheck=1
gpgkey=https://dl.google.com/linux/linux_signing_key.pub
REPO
dnf install -y google-chrome-stable

echo "==> All done"
`

func Run(ctx context.Context, profile, region, instanceID string) (string, error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithSharedConfigProfile(profile),
		config.WithRegion(region),
	)
	if err != nil {
		return "", err
	}
	client := ssm.NewFromConfig(cfg)

	out, err := client.SendCommand(ctx, &ssm.SendCommandInput{
		DocumentName: aws.String("AWS-RunShellScript"),
		InstanceIds:  []string{instanceID},
		Parameters: map[string][]string{
			"commands": {script},
		},
		TimeoutSeconds: aws.Int32(3600),
		CloudWatchOutputConfig: &ssmtypes.CloudWatchOutputConfig{
			CloudWatchLogGroupName:  aws.String("/aws/ssm/AWS-RunShellScript"),
			CloudWatchOutputEnabled: true,
		},
	})
	if err != nil {
		return "", fmt.Errorf("send-command: %w", err)
	}
	return aws.ToString(out.Command.CommandId), nil
}
```

- [ ] **Step 2: Build to verify**

```bash
cd ~/code/devhost
go build ./cmd/ssm/...
```

Expected: no errors.

- [ ] **Step 3: Commit**

```bash
cd ~/code/devhost
git add cmd/ssm/internal/provision/
git commit -m "feat: remote provisioning script via SSM send-command"
```

---

### Task 3: Wire Phase 2+3 into `ssm setup`

**Files:**
- Modify: `cmd/ssm/main.go` — extend `setupCmd()` RunE to call provision.Run then cwlogs.Tail

- [ ] **Step 1: Add Phase 2+3 to setupCmd in main.go**

Add imports:
```go
"os"
"github.com/agenticwrestling/wrangl/cmd/ssm/internal/cwlogs"
"github.com/agenticwrestling/wrangl/cmd/ssm/internal/provision"
```

Append to the `RunE` function in `setupCmd`, after `"// Phase 2 (provisioning) added in M5."` and before `return nil`:

```go
			// Phase 2: Remote provisioning
			fmt.Println("Sending provisioning script...")
			commandID, err := provision.Run(ctx, profile, inst.Region, inst.ID)
			if err != nil {
				return fmt.Errorf("provisioning: %w", err)
			}
			fmt.Printf("Command ID: %s\n", commandID)
			fmt.Println("Tailing CloudWatch Logs (Ctrl-C to detach)...")

			// Phase 3: Tail logs until done
			if err := cwlogs.Tail(ctx, profile, inst.Region, commandID, inst.ID, os.Stdout); err != nil {
				return fmt.Errorf("provisioning failed: %w", err)
			}
```

Remove the `// Phase 2 (provisioning) added in M5.` comment line and `return nil`, replacing with the block above and a final `return nil`.

- [ ] **Step 2: Build**

```bash
cd ~/code/devhost
CGO_ENABLED=0 go build -o ~/bin/ssm ./cmd/ssm
```

Expected: no errors.

- [ ] **Step 3: Install binary**

```bash
cp ~/code/devhost/ssm ~/bin/ssm 2>/dev/null || true
CGO_ENABLED=0 go build -o ~/bin/ssm ./cmd/ssm
~/bin/ssm --help
```

Expected: all three subcommands listed.

- [ ] **Step 4: Commit**

```bash
cd ~/code/devhost
git add cmd/ssm/main.go
git commit -m "feat: ssm setup Phase 2+3 — remote provisioning and CloudWatch log tail"
```

---

### Task 4: Final build and install

- [ ] **Step 1: Full test suite**

```bash
cd ~/code/devhost
go test ./... -v
```

Expected: all non-integration tests pass; integration tests skipped.

- [ ] **Step 2: Build and install**

```bash
cd ~/code/devhost
CGO_ENABLED=0 go build -o ~/bin/ssm ./cmd/ssm
~/bin/ssm --help
~/bin/ssm connect --help
~/bin/ssm tunnel --help
~/bin/ssm setup --help
```

Expected: all help text prints cleanly.

- [ ] **Step 3: Final commit**

```bash
cd ~/code/devhost
git add -A
git commit -m "chore: final build — ssm tool complete"
```

# SSM Tool Design

**Date:** 2026-06-26  
**Status:** Approved

## Overview

A single Go binary (`~/bin/ssm`) that manages AWS EC2 development instances: connecting via SSM Session Manager, tunnelling local ports, and provisioning new instances with a full development toolchain.

## Subcommands

```
ssm connect  [region/name]                         — interactive SSM terminal session
ssm tunnel   [region/name] <rport>[:<lport>] [--open] — SSM port-forward tunnel
ssm setup    [region/name]                         — provision dev toolchain on instance
```

All subcommands:
- Prompt for AWS profile first (lists profiles from `~/.aws/config`)
- Accept optional `region/name` positional arg (e.g. `us-west-2/myhost`); prompt interactively if omitted
- Resolve instance by exact Name tag match; fail clearly if 0 or 2+ matches found

## Architecture

### Language & Structure

Single Go module at `~/code/devhost/cmd/ssm/`. AWS SDK v2 for all AWS API calls (no shelling out to `aws` CLI except where unavoidable). Built with `go build -o ~/bin/ssm ./cmd/ssm`.

```
cmd/ssm/
  main.go          — cobra root command + subcommand registration
  internal/
    aws/
      profiles.go  — list profiles from ~/.aws/config
      resolve.go   — Name tag → (instanceID, region) resolution
      session.go   — SSM session/send-command helpers
      eic.go       — EC2 Instance Connect Endpoint management
      vpc.go       — security group helpers
      cwlogs.go    — CloudWatch Logs tail
    ui/
      picker.go    — fzf-backed or numbered-list interactive picker
      spinner.go   — simple terminal spinner
    ssh/
      config.go    — ~/.ssh/config read/write helpers
      keygen.go    — Ed25519 keypair generation
```

### Dependencies

- `github.com/aws/aws-sdk-go-v2` — core + EC2, SSM, CloudWatch Logs services
- `github.com/spf13/cobra` — subcommand routing
- `github.com/ktr0731/go-fuzzyfinder` — embedded fuzzy picker (no external fzf binary required)

## Subcommand Details

### `ssm connect [region/name]`

1. Prompt profile → prompt/resolve instance
2. Call `aws ssm start-session` by exec-ing the `aws` CLI (SSM Session Manager uses a websocket plugin that the CLI manages; SDK does not replicate this)
3. Inherits stdin/stdout/stderr for a fully interactive shell

### `ssm tunnel [region/name] <rport>[:<lport>] [--open]`

1. Prompt profile → prompt/resolve instance
2. Parse port spec: `3000` means `localhost:3000 → remote:3000`; `3000:8080` means `localhost:8080 → remote:3000`
3. Start SSM port-forwarding session (exec `aws ssm start-session --document-name AWS-StartPortForwardingSession`)
4. Poll `localhost:<lport>` until accepting connections (max 15s)
5. Print ready message; if `--open`, launch `open http://localhost:<lport>` (macOS)
6. Block until user hits Ctrl-C, then clean up

### `ssm setup [region/name]`

#### Phase 1 — EIC Endpoint & SSH Config

1. Describe the instance → get VPC ID, subnet ID, security group IDs
2. Check for existing EIC Endpoint in the VPC; create one if absent (this can take ~60s — show spinner)
3. Ensure the instance's security group has an inbound rule allowing TCP 22 from within the VPC CIDR
4. Generate `~/.ssh/id_ed25519` if it does not exist
5. Call `aws ec2-instance-connect send-ssh-public-key` locally (from the current machine with the selected profile) to register the local Ed25519 public key with the instance — this is valid for 60s and must be repeated each session; the SSH config `ProxyCommand` handles this automatically via `open-tunnel`
6. Upsert a `Host <name>` block in `~/.ssh/config`:
   ```
   Host <name>
     HostName <instance-id>
     User ec2-user
     IdentityFile ~/.ssh/id_ed25519
     ProxyCommand aws ec2-instance-connect open-tunnel \
       --instance-id %h --region <region> --profile <profile>
   ```

#### Phase 2 — Remote Provisioning

Send a single `AWS-RunShellScript` command with the following script:

```bash
#!/bin/bash
set -euo pipefail

# AWS CLI v2
curl -fsSL "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o /tmp/awscliv2.zip
unzip -q /tmp/awscliv2.zip -d /tmp && /tmp/aws/install
rm -rf /tmp/aws /tmp/awscliv2.zip

# ec2-instance-connect
dnf install -y ec2-instance-connect

# Fish shell
dnf install -y fish
chsh -s /usr/bin/fish ec2-user

# Go
dnf install -y golang

# Rust
sudo -u ec2-user sh -c 'curl --proto =https --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --no-modify-path'

# Zed remote server
ARCH=$(uname -m)
RELEASE=$(curl -sf https://api.github.com/repos/zed-industries/zed/releases/latest | grep tag_name | cut -d'"' -f4)
curl -fsSL "https://github.com/zed-industries/zed/releases/download/${RELEASE}/zed-remote-server-linux-${ARCH}.gz" \
  | gunzip > /usr/local/bin/zed-remote
chmod +x /usr/local/bin/zed-remote

# Headless Chrome
cat > /etc/yum.repos.d/google-chrome.repo <<'EOF'
[google-chrome]
name=google-chrome
baseurl=http://dl.google.com/linux/chrome/rpm/stable/x86_64
enabled=1
gpgcheck=1
gpgkey=https://dl.google.com/linux/linux_signing_key.pub
EOF
dnf install -y google-chrome-stable
```

The local binary sends its own SSH public key as a parameter to the send-command so it can be placed on the instance.

#### Phase 3 — CloudWatch Log Tail

After `send-command` returns a Command ID:

1. Construct expected log stream name: `<command-id>/<instance-id>/aws-runshellscript/0.awsrunshellscriptplugin`
2. Poll `FilterLogEvents` on log group `/aws/ssm/AWS-RunShellScript` every 2s, printing new events in order
3. Exit when the command reaches a terminal state (Success / Failed / TimedOut)
4. Print final status and exit code; non-zero command exit code causes `ssm setup` to exit non-zero

## Error Handling

| Situation | Behaviour |
|---|---|
| 0 instances match Name tag | Fatal error with region searched |
| 2+ instances match Name tag | Fatal error listing all matching (region, ID) pairs |
| EIC Endpoint creation fails | Fatal error with AWS error message |
| send-command times out (default 3600s) | Print timeout message, exit non-zero |
| SSH config Host block already exists | Overwrite silently if instance ID changed, skip if identical |

## Build & Install

```
cd ~/code/devhost
go build -o ~/bin/ssm ./cmd/ssm
```

No Makefile needed yet. Binary is statically linked (CGO_ENABLED=0) for portability.

## Out of Scope

- Multi-instance fan-out for setup
- Windows EC2 instances
- Non-ec2-user default login names (can add `--user` flag later)
- `ssm stop` / instance lifecycle management

# devhost

CLI tools for managing EC2 development instances.

## ssm

A single binary for connecting to, tunnelling into, and provisioning EC2 instances via AWS SSM and EC2 Instance Connect — no open inbound ports required.

### Prerequisites

- [AWS CLI v2](https://docs.aws.amazon.com/cli/latest/userguide/install-cliv2.html) with the [Session Manager plugin](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html)
- AWS profiles configured in `~/.aws/config`
- Go 1.21+
- [Task](https://taskfile.dev) (`brew install go-task`)

### Build & Install

```sh
task install   # builds to ./build/ssm then copies to ~/bin/ssm
```

Or just build:

```sh
task build     # ./build/ssm
```

### Usage

All subcommands prompt for an AWS profile first, then accept an optional `region/name` argument (e.g. `us-west-2/myhost`). If omitted, a fuzzy picker lists all running instances in the default region.

#### `ssm connect [region/name]`

Open an interactive SSM Session Manager terminal on the instance.

```sh
ssm connect
ssm connect us-west-2/myhost
```

#### `ssm tunnel [region/name] <rport>[:<lport>] [--open]`

Forward a remote port to localhost via SSM. `--open` launches the browser once the tunnel is ready.

```sh
ssm tunnel us-west-2/myhost 3000          # localhost:3000 → remote:3000
ssm tunnel us-west-2/myhost 3000:8080     # localhost:8080 → remote:3000
ssm tunnel us-west-2/myhost 3000 --open   # opens http://localhost:3000
```

#### `ssm setup [region/name]`

Provision a fresh Amazon Linux instance with a full development toolchain:

- AWS CLI v2
- EC2 Instance Connect
- Fish shell (set as default)
- Go
- Rust
- Zed remote server
- Headless Chrome

Also configures:
- EC2 Instance Connect Endpoint in the instance's VPC (created if absent)
- SSH ingress rule (port 22 from VPC CIDR) on the instance's security group
- Local Ed25519 keypair (`~/.ssh/id_ed25519`) generated if absent
- `~/.ssh/config` `Host` block so `ssh myhost` and Zed remote just work

Streams CloudWatch Logs output until provisioning completes.

```sh
ssm setup us-west-2/myhost
```

### Development

```sh
task test    # run tests
task clean   # remove ./build
```

## wrangl

A single binary that runs as a **controller** (SQLite, embedded NATS + JetStream, HTTP API) or a
**runner** (enrols, applies a mise-based template, drives a coding agent through a hierarchical
task stack), plus the client CLI/TUI. Design: [`docs/superpowers/specs/2026-10-31-wrangl-design.md`](docs/superpowers/specs/2026-10-31-wrangl-design.md).

Status: **M1–M10 built**, tested against fakes for AWS, GitHub and the agents (see the
install guide's first-run checklist for what still needs a real account).
- You give wrangl a hierarchical, ranked task stack.
- It launches tagged EC2 runners, which are terminated after 30 idle minutes, and installs
  your template's toolchain for an unprivileged `agent` user.
- It drives a coding agent through each queued subtree in a PTY: headless, or as one
  interactive session with optional Remote Control. Agents manage their own subtasks over MCP.
- It makes a checkpoint commit per subtask, opens a PR, has a review run assess it, and
  auto-merges low-risk work or asks you.
- You run it from the CLI, a TUI, or an installable mobile PWA with web push, behind
  Cloudflare Access. Your own agents can manage the backlog through the controller's MCP
  endpoint (`wrangl mcp`).

**Install:** [`docs/wrangl/INSTALL.md`](docs/wrangl/INSTALL.md) covers AWS, the controller
host, the GitHub Apps, the Claude token, templates, Cloudflare, and teardown.
AWS setup (instance roles, security group, teardown): [`docs/wrangl/aws-iam.md`](docs/wrangl/aws-iam.md).

```sh
task build-wrangl      # build/wrangl + build/wrangl-linux-{aarch64,x86_64}
task sqlc              # regenerate cmd/wrangl/internal/db/dbgen after editing SQL
task sqlc-check        # fail if dbgen is stale

build/wrangl --config ./c.yaml controller init --data-dir ./data
build/wrangl --config ./c.yaml controller serve
curl --unix-socket /run/wrangl/api.sock http://x/healthz

build/wrangl template create -f template.yaml
build/wrangl fleet launch --template go-claude     # tagged EC2 instance, enrols itself
build/wrangl fleet list
build/wrangl fleet teardown                         # terminate every tagged instance
```


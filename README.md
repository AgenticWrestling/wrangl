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

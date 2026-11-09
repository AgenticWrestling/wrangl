# Your own dev instances: `wrangl dev`

`wrangl dev` is for EC2 instances that are yours (a dev box, the controller itself, a runner you want
to poke at), as opposed to the runners the controller launches. It talks to AWS with your own
credentials and **never needs a controller**, so it works before one exists (INSTALL.md step 5). The reasoning is in [ADR 0010](../adr/0010-fold-ssm-into-wrangl.md).

You need the AWS CLI v2 and, for `connect` and `tunnel`, the
[Session Manager plugin](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html).
The instance needs the SSM agent and `AmazonSSMManagedInstanceCore` ([aws-iam.md](../aws-iam.md)).

## Choosing a target

Every command takes an optional `TARGET`: `[region/][user@]name` (the instance's `Name` tag), an
instance id (`i-0abc...`), or an alias from `~/.ssh/config` written by `wrangl dev ssh`. Without a
target you pick from a list. The profile is `--profile`, else `AWS_PROFILE`, else a picker over
`~/.aws/config` (most recently used first, history in `~/.config/wrangl/dev-history.json`); an expired
SSO login is refreshed for you. The region is `--region`, the `region/` prefix, `AWS_REGION`, then
the profile's region.

## Commands

| Command | What it does |
|---|---|
| `wrangl dev list [REGION] [--state running,stopped\|all]` | name, id, state, private IP |
| `wrangl dev connect [TARGET]` | interactive SSM shell (`aws ssm start-session`) |
| `wrangl dev tunnel [TARGET] RPORT[:LPORT] [--open]` | forward `localhost:LPORT` to the instance's `RPORT` over SSM; Ctrl-C closes it |
| `wrangl dev ssh [TARGET] [-- ARGS]` | ssh through EC2 Instance Connect (see below) |
| `wrangl dev ssh-config [TARGET]` | write the `~/.ssh/config` entry without connecting |
| `wrangl dev start [TARGET]` / `stop` | start a stopped instance and wait for it, or stop one |
| `wrangl dev setup [TARGET]` | install a toolchain with mise (below) |
| `wrangl fleet connect NAME`, `fleet tunnel NAME RPORT[:LPORT]` | the same for a runner's instance, found through the controller |

### ssh

`dev ssh` generates a dedicated key (`~/.ssh/wrangl_dev_ed25519`; your own `id_ed25519` is never
touched), pushes its public key with EC2 Instance Connect (valid 60 seconds), writes a `Host` block and
runs `ssh`. The block's `ProxyCommand` is `wrangl dev proxy`, which pushes a fresh key at every
connection, so a plain `ssh NAME`, `scp` or an editor's remote mode keeps working later. No port 22
is opened to the internet. If the instance has no public address it needs an EC2 Instance Connect
Endpoint in its VPC and a security group rule for port 22 from the VPC: `wrangl dev ssh --prepare-network`
creates both (an endpoint is billed infrastructure, so it is never created implicitly). The instance
needs the `ec2-instance-connect` package (`dev setup` installs it).

## Installing software: `wrangl dev setup`

```sh
wrangl dev setup us-west-2/mybox                 # the default toolchain
wrangl dev setup mybox --with android,browser    # plus overlays
wrangl dev setup mybox --file my-box.yaml        # your own profile
wrangl dev setup mybox --template go-claude      # the toolchain of a runner template
wrangl dev setup mybox --dry-run                 # print the script, change nothing
```

Software is installed by [mise](https://mise.jdx.dev), the same way runner templates do it. A
profile is the two template fields that install software:

```yaml
mise_toml: |        # what mise installs, for the user
  [tools]
  go = "1.26"
setup_script: |     # OS-level prerequisites, bash, as root; reruns only when it changes
  dnf install -y git
```

`dev setup` renders one script from the profile and runs it as root on the instance: it installs
mise to `/usr/local/bin/mise` if missing, runs `setup_script` if its content changed, writes the
`mise_toml` to `~/.config/mise/conf.d/wrangl-dev.toml` of `--user` (default `ec2-user`; your own
`config.toml` is left alone, and a runner's template config is not touched) and runs `mise install`
as that user. Running it again is safe and fast.

**The default profile** is Go, Rust (stable), Node (LTS), Python, uv, Java (Corretto 25), the AWS CLI
and the CLI tools a runner's first shell installs (bat, lsd, dust, duf, fd, ripgrep, tealdeer);
its setup script only installs git, tar, unzip, gzip, CA certificates and `ec2-instance-connect`.
Overlays for `--with`: `android` (Corretto and the Android SDK), `browser` (Playwright and Chromium's
libraries), `fish` (the fish shell). The android and fish overlays use mise's `vfox`/`github`
backends and have not been tried on a real instance; pin or replace them in your own file if they
misbehave.

Transport: `--via ssm` (default) uses SSM Run Command, so nothing needs to be open. Output streams
from CloudWatch Logs (group `/aws/ssm/AWS-RunShellScript`; the instance role needs `logs:CreateLogStream`
and `logs:PutLogEvents`), and without that you get the last 24,000 characters when it ends.
`--via ssh` uses Instance Connect and ssh instead; `--ssh HOST` applies the profile over plain ssh to
any machine you can reach, AWS or not. `--idle-stop 4h` also installs a timer that powers the instance
off (an EBS-backed instance then stops) after that long with no load and nobody logged in.

Security groups edited by an earlier standalone tool may carry an all-ports rule described `ssm-managed`: `dev setup` warns about it and `--revoke-legacy-rules` removes it.

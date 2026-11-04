# Installing wrangl

*wrangl: **W**ork, **R**un, **A**sk, **N**udge, **G**ate, **L**and.*

This guide takes you from an empty AWS account to a runner finishing a task and pushing a
branch to GitHub. Do the steps in order. Each step ends with a check.

> **What has been verified, and what hasn't.** Everything in wrangl is covered by tests that
> run a real controller and a real runner daemon in one process. Five pieces have **not**
> yet run against the real service, because the build sandbox had no access to it:
> - EC2 (launch, tags, terminate)
> - the IAM policy below
> - the GitHub App (creation through the manifest flow, installation, merging)
> - `mise install`
> - Claude Code
>
> Treat your first launch, first `repo link` and first run as the real test, and watch them.
> Step 11 lists what to look at.

**Contents**

0. [What you end up with](#0-what-you-end-up-with)
1. [Prerequisites](#1-prerequisites)
2. [Build the binaries](#2-build-the-binaries)
3. [AWS network](#3-aws-network)
4. [IAM roles](#4-iam-roles)
5. [The controller host](#5-the-controller-host)
6. [GitHub Apps](#6-github-apps)
7. [The Claude Code token](#7-the-claude-code-token)
8. [A runner template, and the first runner](#8-a-runner-template-and-the-first-runner)
9. [The first project and run](#9-the-first-project-and-run)
10. [Cloudflare Zero Trust](#10-cloudflare-zero-trust-remote-cli-now-pwa-in-m8)
11. [First-run checklist](#11-first-run-checklist-the-unverified-parts)
12. [Day 2: backups, upgrades, teardown](#12-day-2-backups-upgrades-teardown)
13. [Troubleshooting](#13-troubleshooting)

---

## 0. What you end up with

```
                Cloudflare Zero Trust ──▶ cloudflared ─┐
                                                      │ 127.0.0.1:8080 (API + PWA)        
  VPC, private subnet + NAT gateway                   ▼
  ┌──────────────────────────────────────────────────────────────┐
  │  controller (t4g.small)       runners (launched on demand)   │
  │  wrangl controller serve ◀────  wrangl runner run             │
  │    :4222 NATS (TLS)    ◀──────  enrol on :4223, then NATS     │
  │    :4223 enrol + binary          agent runs as user `agent`  │
  └──────────────────────────────────────────────────────────────┘
       all EC2 calls use instance roles; no AWS keys anywhere
```

- The controller launches runners itself, sized by each template's minimum vCPU, RAM and disk.
- Runners that sit idle for 30 minutes are terminated.
- Every runner instance, volume and network interface is tagged `wrangl:managed=true`, so
  `wrangl fleet teardown` can always find and remove them.

## 1. Prerequisites

| You need | For |
|---|---|
| An AWS account with an admin you can use from a laptop (`aws` CLI v2, the session-manager plugin) | steps 3–5 |
| A GitHub account or org where you can create a GitHub App and install it on repos | step 6 |
| A Claude Pro, Max, Team or Enterprise plan, with Claude Code installed on your laptop | step 7 |
| A Cloudflare account with Zero Trust (the free plan is enough), and a domain on Cloudflare | step 10 (optional for now) |
| Go 1.26+, [Task](https://taskfile.dev) and git on your laptop | step 2 |
| This repo's `ssm` tool (`task install`) | connecting to the controller without opening port 22 |

Pick a region and use it throughout:

```sh
export AWS_REGION=us-west-2
```

## 2. Build the binaries

```sh
git clone https://github.com/AgenticWrestling/wrangl && cd wrangl
task build-wrangl
ls build/
# wrangl  wrangl-linux-aarch64  wrangl-linux-x86_64
```

`build/wrangl` is for your laptop, as the CLI. The two `wrangl-linux-*` binaries go on the
controller, which serves them to runners as they boot.

## 3. AWS network

> **Shortcut for sections 3–5:** `deploy/wrangl/controller.sh up` does all of it in one go.
> It creates the security group, both IAM roles (the policy is read from
> [`aws-iam.md`](aws-iam.md)), a private artifact bucket and the controller instance, whose
> first boot installs, configures and starts wrangl. Run `task build-wrangl` first, then:
> ```sh
> AWS_REGION=us-west-2 VPC_ID=vpc-... SUBNET_ID=subnet-... deploy/wrangl/controller.sh up
> ```
> It's idempotent: run it again and it finds what exists. Other modes:
> - `controller.sh upgrade` ships new binaries and restarts the controller over SSM. Runners
>   then upgrade themselves.
> - `controller.sh status` shows what exists.
> - `controller.sh down` terminates everything and deletes what `up` made, after you confirm.
>
> It doesn't create the VPC or NAT gateway, and it stops if the subnet has no default
> route. Then continue at section 6. The manual steps below are the same thing by hand.

Runners and the controller share one VPC subnet and one security group. Runners need outbound
internet access for mise, GitHub and the Claude API, so use a **private subnet with a NAT
gateway**. Nothing needs inbound access from the internet.

If you already have a VPC with a private subnet and NAT, reuse it. Otherwise, create one in the
VPC console with the "VPC and more" wizard: 1 AZ, 1 public + 1 private subnet, 1 NAT gateway.

```sh
export VPC_ID=vpc-...        # your VPC
export SUBNET_ID=subnet-...  # a PRIVATE subnet with a route to a NAT gateway

# One security group for controller + runners; members may reach each other on 4222-4223.
export SG_ID=$(aws ec2 create-security-group --group-name wrangl \
  --description "wrangl controller and runners" --vpc-id "$VPC_ID" \
  --query GroupId --output text)
aws ec2 authorize-security-group-ingress --group-id "$SG_ID" \
  --ip-permissions "IpProtocol=tcp,FromPort=4222,ToPort=4223,UserIdGroupPairs=[{GroupId=$SG_ID}]"
```

**Check:** the SG has one inbound rule, TCP 4222–4223 from itself:
`aws ec2 describe-security-groups --group-ids "$SG_ID"`

> The enrolment check compares a request's source IP with the instance's private IP. Runners
> must reach the controller **directly inside the VPC**, with no NAT, proxy or load balancer
> in between. Same subnet, or routable subnets in the same VPC.

## 4. IAM roles

Two roles, both trusted by EC2.

```sh
cat > ec2-trust.json <<'EOF'
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}
EOF
ACCOUNT=$(aws sts get-caller-identity --query Account --output text)

# Runner role: Session Manager only (so you can `ssm connect` to a runner to debug).
aws iam create-role --role-name wrangl-runner --assume-role-policy-document file://ec2-trust.json
aws iam attach-role-policy --role-name wrangl-runner \
  --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore
aws iam create-instance-profile --instance-profile-name wrangl-runner
aws iam add-role-to-instance-profile --instance-profile-name wrangl-runner --role-name wrangl-runner

# Controller role: launch, tag and terminate wrangl runners only.
aws iam create-role --role-name wrangl-controller --assume-role-policy-document file://ec2-trust.json
aws iam attach-role-policy --role-name wrangl-controller \
  --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore
```

Copy the policy JSON from [`aws-iam.md`](aws-iam.md) into `controller-policy.json`. Replace
`REGION` and `ACCOUNT`, then attach it:

```sh
sed -i "s/REGION/$AWS_REGION/g; s/ACCOUNT/$ACCOUNT/g" controller-policy.json
aws iam put-role-policy --role-name wrangl-controller --policy-name wrangl-controller \
  --policy-document file://controller-policy.json
aws iam create-instance-profile --instance-profile-name wrangl-controller
aws iam add-role-to-instance-profile --instance-profile-name wrangl-controller --role-name wrangl-controller
```

**Check:** both instance profiles exist:
`aws iam list-instance-profiles --query 'InstanceProfiles[].InstanceProfileName'`

## 5. The controller host

### 5.1 Launch it

A small arm64 instance is plenty. **Don't tag it `wrangl:managed`**: that tag marks runners,
and `fleet teardown` would terminate the controller too.

```sh
aws ec2 run-instances \
  --image-id resolve:ssm:/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64 \
  --instance-type t4g.small --subnet-id "$SUBNET_ID" --security-group-ids "$SG_ID" \
  --iam-instance-profile Name=wrangl-controller \
  --metadata-options HttpTokens=required,HttpEndpoint=enabled \
  --block-device-mappings '[{"DeviceName":"/dev/xvda","Ebs":{"VolumeSize":30,"VolumeType":"gp3","Encrypted":true}}]' \
  --tag-specifications 'ResourceType=instance,Tags=[{Key=Name,Value=wrangl-controller}]' \
  --query 'Instances[0].[InstanceId,PrivateIpAddress]' --output text
```

Note the private IP (for example `10.0.1.5`): runners will be told to dial it. Connect with
this repo's tool:

```sh
ssm connect "$AWS_REGION/wrangl-controller"
```

### 5.2 Copy the binaries over

Use `ssm setup "$AWS_REGION/wrangl-controller"` once to get an SSH config entry that goes
through EC2 Instance Connect, then:

```sh
scp build/wrangl-linux-aarch64 build/wrangl-linux-x86_64 wrangl-controller:/tmp/
```

If you'd rather not use SSH, `aws s3 cp` the files through a bucket the controller can read.

### 5.3 Install

Run these on the controller:

```sh
sudo install -m 0755 /tmp/wrangl-linux-aarch64 /usr/local/bin/wrangl
sudo useradd --system --home-dir /var/lib/wrangl --shell /sbin/nologin wrangl
sudo install -d -o wrangl -g wrangl -m 0750 /etc/wrangl /var/lib/wrangl
sudo install -d -o wrangl -g wrangl -m 0750 /var/lib/wrangl/bin
sudo install -o wrangl -g wrangl -m 0755 /tmp/wrangl-linux-aarch64 /tmp/wrangl-linux-x86_64 /var/lib/wrangl/bin/
sudo usermod -aG wrangl ec2-user   # lets you use the CLI on this host (log out and back in)

sudo -u wrangl wrangl --config /etc/wrangl/controller.yaml controller init --data-dir /var/lib/wrangl
```

`init` creates:
- the database
- the NATS operator, account and signing keys, and the TLS CA (`/var/lib/wrangl/nats/`)
- the per-install task-id alphabet
- this controller's id (`wr-…`), which tags its instances
- a config skeleton at `/etc/wrangl/controller.yaml`

It's safe to run again; it never overwrites anything.

### 5.4 Configure

Edit `/etc/wrangl/controller.yaml`. Fill in the AWS section, and put the controller's
**private IP** on the NATS and enrol listeners (launches refuse a loopback address, because
runners could never reach it):

```yaml
data_dir: /var/lib/wrangl
listen:
  http: 127.0.0.1:8080          # API (cloudflared connects here)
  unix: /run/wrangl/api.sock    # local CLI
  nats: 10.0.1.5:4222           # <- controller private IP
  enroll: 10.0.1.5:4223         # <- controller private IP
aws:
  region: us-west-2
  subnet_ids: [subnet-...]      # the private subnet from step 3
  security_group_id: sg-...     # the wrangl SG
  runner_instance_profile: wrangl-runner
cloudflare: { team: "", aud: "", owner_email: "" }   # step 10 / M8
web: { public_url: "" }            # required for GitHub setup, step 6
```

You can read the private IP on the host with:
`TOKEN=$(curl -sX PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60'); curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/local-ipv4`

### 5.5 Run it under systemd

```sh
sudo tee /etc/systemd/system/wrangl-controller.service >/dev/null <<'EOF'
[Unit]
Description=wrangl controller
After=network-online.target
Wants=network-online.target

[Service]
User=wrangl
Group=wrangl
ExecStart=/usr/local/bin/wrangl --config /etc/wrangl/controller.yaml controller serve
Restart=always
RestartSec=5
RuntimeDirectory=wrangl
RuntimeDirectoryMode=0750

[Install]
WantedBy=multi-user.target
EOF
sudo systemctl daemon-reload
sudo systemctl enable --now wrangl-controller
```

**Check:**

```sh
journalctl -u wrangl-controller -n 20     # "controller serving ... aws=true"
curl --unix-socket /run/wrangl/api.sock http://x/healthz
# {"db":"ok","jetstream":"ok","nats":"ok"}
```

Without root (a dev box), use the `systemd --user` unit in `docs/wrangl/systemd/wrangl-controller.user.service`
instead; it logs to `~/.local/share/wrangl/controller.log` and restarts on failure.

From here on, `wrangl` commands run on the controller host go to the local socket. Step 10
covers running them from your laptop.

## 6. GitHub: one App per account

wrangl needs one GitHub App for each GitHub account that owns repositories it works on (your
personal account, and any organisation). A GitHub App lives in one account and by default can only
be installed there, so a repo in another organisation needs that organisation added too. You
don't create the App by hand: the web app creates it through GitHub's manifest flow.

The App clones and pushes branches, opens PRs, reads issues and CI checks, merges PRs, and
receives webhooks. The controller mints a fresh installation token for each run, scoped to **one**
repo with `contents:write` and `pull_requests:write`. Only the runner daemon (root) uses it for
clone and push; the agent never sees it.

### 6.1 Prerequisite: `web.public_url`

GitHub redirects your browser back to the controller and sends webhooks to it, so the manifest
carries the controller's public address. Set it in `/etc/wrangl/controller.yaml` and restart:

```yaml
web: { public_url: "https://wrangl.example.com" }   # the hostname from step 10
```

Without it the Setup page shows "web.public_url is not set in controller.yaml" instead of a
Create App button. (Step 10 sets up the hostname and Cloudflare Access; do that first, or put in
the address you will use.)

### 6.2 Add an account and create its App

1. In the web app open **Settings → GitHub accounts → Manage** (the `/setup` page).
2. Type the GitHub user or organisation and press **Add account**. Adding an account with no App
   sends you straight on to GitHub with the manifest prefilled.
3. On GitHub, confirm the App's name (`wrangl-<login>`; names are global on GitHub) and press
   **Create GitHub App**.
4. GitHub returns you to wrangl, which stores the App's id and private key under the data
   directory (`secrets/gh-<login>.pem`). It then **sends you on to install the App**: choose **Only select
   repositories** and pick the repos wrangl may work on.

**Check:** the account's row shows **✓ Ready · N repositories**. Other states:

| Status | Meaning | What to do |
|---|---|---|
| ○ No App yet | account added, App not created | **Create App** |
| ! Not installed / Installed on no repositories | the App exists but has no repos | **Install**, then pick repos |
| ! Credentials rejected | GitHub refused the stored key (App deleted, key revoked) | **Re-authorize** |
| ✓ Ready · N repositories | working | **Repositories** manages which repos |

**Re-authorize** creates a fresh App (with a new name) and replaces the stored one; the old App
stays on GitHub until you delete it. Use it when credentials are rejected. The **⋯** menu on
a row also has **Check again**, **Install on more repositories**, **Re-authorize…** and
**Remove** (unlink the account; its repos stop working until it is added again).

If you used an earlier version with a single App (the Setup page's, or `github.author_app` in
`controller.yaml`), the controller adopts it into the account that owns it at start. No action
is needed. Delete the legacy `github.author_app` block only if you like; it is no longer how
Apps are set up.

Then link repos with `wrangl repo link` (step 9). The webhook URL is `<public_url>/gh/webhook`;
GitHub generates the secret for each App and wrangl verifies deliveries against it, so there is no
webhook secret to create or paste. Step 10 (item 4, "Webhook bypass") lets that path through Cloudflare Access.

### 6.3 Advanced: the reviewer App (optional)

A GitHub App can't approve a PR it opened. That only matters when a repository **requires
approving reviews** before merge. In that case either:

- add the account's App to the repository ruleset's **bypass list** (nothing else to do), or
- create a separate **reviewer App**: on the account's row open **⋯ → Advanced: reviewer App →
  Create the reviewer App** and install it on the same repos. wrangl then uses it to approve,
  merge and read PRs for that account, and the first App only authors.

Skip this if your repos don't require approving reviews: the one App merges for you. It hasn't
been verified yet whether an App's approval satisfies a "required approvals" rule, so the bypass
list is the safer choice.

## 7. The Claude Code token

Runners run Claude Code headless, authenticated by a long-lived token tied to your plan.
`setup-token` needs a browser, so run it on your laptop:

```sh
claude setup-token          # opens a browser, then prints a token valid for one year
```

Then store it on the controller. `secret set` reads stdin and writes
`/var/lib/wrangl/secrets/claude_oauth_token` with mode 0600:

```sh
sudo -u wrangl wrangl --config /etc/wrangl/controller.yaml controller secret set claude_oauth_token
# paste the token, then Ctrl-D
sudo -u wrangl wrangl --config /etc/wrangl/controller.yaml controller secret list
```

The controller sends it to each run in the agent's process environment only. It's never
written to a runner's disk. Every runner draws on this one plan's usage limits.

## 8. A runner template, and the first runner

A template says what to install on a runner (a mise config plus an optional root setup
script), how big the machine must be, and how to start the agent. Save this as
`go-claude.yaml`:

```yaml
name: go-claude
arch: arm64
min: { vcpu: 2, ram: 8GiB, disk: 40GiB }
instance_types: [m7g.large, m7g.xlarge]     # optional; omit to pick the smallest shape that fits (no local-NVMe types: im4gn costs ~2x m7g)
max_runners: 2
idle_timeout: 30m                           # terminated after this long idle (default 30m)
setup_script: |                             # runs as root, only when it changes
  dnf install -y git make gcc
mise_toml: |                                # installed for the unprivileged `agent` user
  [settings]
  npm.package_manager = "npm"   # mise's default (aube) skips the postinstall that fetches claude's native binary

  [tools]
  go = "1.26"
  node = "24"
  "npm:@anthropic-ai/claude-code" = { version = "latest", npm_args = "--ignore-scripts=false" }
agent:
  start:  ["claude", "-p", "--session-id", "{{.SessionUUID}}", "--permission-mode", "bypassPermissions", "{{.Prompt}}"]
  resume: ["claude", "-p", "--resume", "{{.SessionUUID}}", "--permission-mode", "bypassPermissions", "{{.Prompt}}"]
  secrets: { CLAUDE_CODE_OAUTH_TOKEN: claude_oauth_token }
  auth: { secret: claude_oauth_token, command: claude setup-token }   # how you collect it (see below)
  turn_timeout: 2h
  files:                                    # written into the worktree before the first turn
    - path: .mcp.json                       # the work-stack tools (task_complete, ask_question, ...)
      content: '{"mcpServers":{"wrangl":{"command":"wrangl","args":["runner","mcp"]}}}'
    - path: .claude/settings.local.json     # hooks report back to the controller
      content: |
        {"hooks": {
          "PostToolUse":  [{"hooks":[{"type":"command","command":"wrangl runner hook PostToolUse"}]}],
          "Notification": [{"hooks":[{"type":"command","command":"wrangl runner hook Notification"}]}],
          "Stop":         [{"hooks":[{"type":"command","command":"wrangl runner hook Stop"}]}]},
         "enableAllProjectMcpServers": true}
```

Notes:
- **One Claude session per assignment.** The first subtask uses `start` with a fresh session
  id, and later subtasks `resume` it, so the agent keeps context across subtasks.
- **What the agent runs as.** `bypassPermissions` runs as the unprivileged `agent` user on a
  disposable box that can't read the runner's NATS credentials or the GitHub token. That box
  is the security boundary, not Claude's permission prompts.
- **How a subtask completes.** The agent calls the `task_complete` MCP tool. If it ends a turn
  without doing so, the runner nudges it once, then asks you (`wrangl question list`): retry,
  mark complete, or fail the run. Agents without MCP can set `completion: exit`, so that a turn
  exiting 0 completes the subtask. Either way the runner checkpoint-commits whatever the agent
  left uncommitted.
- **What the agent can do with the stack.** It can read the whole project, and create,
  reorder, complete or add precursors only inside its assignment. Cancelling or deleting a
  task it didn't create becomes an approval question for you. `ask_question` blocks until you
  answer with `wrangl question answer <id> <option>` (or from the web app in M8).

Create the template and launch a runner:

```sh
wrangl template create -f go-claude.yaml
wrangl fleet launch --template go-claude
wrangl fleet list          # watch: launching -> enrolling -> provisioning -> idle
```

To follow a runner's progress:
- `fleet show <runner>` shows whether the template applied (`applied v1 of v1`).
- On failure it shows the last error and the tail of the output, and cordons the runner.
- Booting, enrolling and `mise install` take a few minutes the first time.

**Check:** `wrangl fleet list` shows the runner `idle` with CPU, memory and disk figures.

### Research citation audit (optional)

`wrangl project edit PROJ --citation-audit` turns on an audit for the project's **research** tasks.
After the normal risk review of a research PR, wrangl queues one more read-only run (a `review` run
whose prompt lists every URL the PR adds, taken from the diff). The agent fetches each URL, checks it
loads and supports the claim it is cited for, and reports `verified`, `unsupported` or `unreachable`
for each with the `submit_citation_audit` tool; any URL it fails to report on counts as "not checked".
Auto-merge then requires every citation verified. Otherwise (or if the audit run ends without reporting)
the PR question for the owner gets a **Citation audit** section naming what to look at. The audit
re-runs for each new head commit. It needs an agent that can fetch web pages; the review run uses the
same template and agent as the PR's work run.

### 8.0 Agent definitions

The `agent:` block above is stored as a named **agent definition** (here `claude`), separate from
the template: the template says what machine to build and which agents may run on it, the agent
says how to start and configure one (start/resume, secrets, hook and MCP files). You rarely need
to think about it: creating a template with an `agent:` block creates (or reuses, when identical)
the agent and allows it on the template, and existing templates were migrated the same way with
no change in behaviour.

```
wrangl agent list                                   # name, version, templates that allow it
wrangl agent show claude                            # the spec
wrangl agent create codex -f codex-agent.yaml       # a spec: the contents of an `agent:` block
wrangl agent edit claude -f claude-agent.yaml       # takes effect for runs assigned from now on
wrangl agent allow codex --template go-claude       # or list `agents: [claude, codex]` in the template file
wrangl agent disallow codex --template go-claude
wrangl project edit myproj --agent claude           # the project's default agent
wrangl task queue SQID --agent codex                # override for this run
```

An agent is chosen as: the `--agent` override, else the project's default if the run's template
allows it, else the template's only allowed agent. A template that allows several agents with no
usable default needs `--agent`. Asking for an agent the template does not allow is refused when you
queue, naming what is allowed. Editing an agent does not bump the template version, so no runner
reinstalls anything. The agent's binary is still installed by the template (`mise_toml` /
`setup_script`), so allow an agent only on templates that install it. Secrets belong to the agent:
a missing one is reported as `agent NAME needs secret ...`.

**Collecting the Claude token.** The token is owned by the agent, not stored by hand. On any
machine where `claude` is logged in to your plan, run `claude setup-token`, then:

```
claude setup-token | wrangl agent auth claude      # shows the step, stores the last line as claude_oauth_token
# or, equivalently:  claude setup-token | wrangl secret set claude_oauth_token
```

(`wrangl controller secret set` still works on the controller host.) **Settings -> Setup ->
Agent credentials** lists every secret each agent needs with `✓ set` or `! missing` and the
command to fix it; a missing secret also holds the run with `agent claude needs secret ...`.
Other agents declare the same `auth:` section (`secret`, `command`, `note`) in their spec.

### 8.2 Remote runners (Mac, Android emulator) (experimental)

A template with `mode: remote` and a `connection:` block describes a **target** machine that runs
no wrangl binary (see `templates/remote-mac.yaml`, `remote-android.yaml` and
[ADR 0002](adr/0002-remote-runners.md)). The agent runs on a **driver**, a session of an ordinary
runner template named in `connection.driver_template`, with the repo checked out there. It drives
the target with two tools served by the driver's daemon: `target_exec` (a shell command over ssh, or
`adb shell`) and `target_put` (scp / `adb push` from the checkout). The SSH key is a controller
secret (`wrangl secret set NAME`), is held only by the daemon (a 0700 temp dir removed when the run
ends) and never reaches the agent. The target is reached through an SSM port-forward started by the
driver (`aws ssm start-session`, so the driver needs the AWS CLI + the Session Manager plugin and
`ssm:StartSession`), or directly with `via: direct`.

A run on a remote template takes a session on the target (`max_sessions`) and a session on a driver
at the same time. The controller launches a target when queued runs need one, within the project's
cap for the template (default 0: opt in with `wrangl project edit PROJ --limit mac=1`), probes it
every loop, marks it `offline` after three failed probes, reaps it after `idle_timeout`, and fails the
runs on it as `target lost` if it is gone or unreachable for two minutes. `wrangl target list` shows
targets and their sessions; `wrangl target terminate NAME` stops one now.

**Android emulator:** see [`android.md`](android.md) (KVM-capable instance and its cost, the AMI, adb
access and a build-and-install smoke test).

**EC2 Mac (`templates/remote-mac.yaml`).** A Mac instance runs on a Dedicated Host with a
**24-hour minimum allocation**: one short run bills a full day and stopping early saves nothing. So
a project's cap on the Mac template stays 0 (opt in with `wrangl project edit PROJ --limit mac=1`),
the controller logs a "Cost warning" note every time it launches one (also shown in the controller
agent's `fleet.launch` result), and the template's `idle_timeout` is 24 h so back-to-back runs share
the host. wrangl launches the instance; allocating the host and choosing the macOS AMI is yours.

Not built yet: the plain `ssh target` escape hatch and signing forwarder, the Runners-page card and
ssh terminal for targets, and EC2 Instance Connect keys.

### 8.1 Other agents, interactive sessions and Remote Control (experimental)

`docs/wrangl/templates/` holds example templates. Before picking one, read
[`agents.md`](agents.md): it covers how each agent's hooks, session ids and MCP plug in, and
which credentials to use.

| File | Status | What it is |
|---|---|---|
| `claude-headless.yaml` | the one above | `claude -p` per subtask, one resumed session |
| `claude-interactive.yaml` | doc-checked, not run | one interactive Claude session per assignment, with Remote Control |
| `codex.yaml` | source-checked, not run | `codex exec --json`, resumed by thread id; hooks and MCP in `~/.codex` |
| `antigravity.yaml` | **largely unverified** | Antigravity's `agy` CLI with ADC or a Gemini API key |
| `gemini.yaml` | doc-checked, not run | Gemini CLI headless with an API key or Vertex |

The unverified ones are tested only against fake agents. Try them on one runner first:

```sh
# interactive Claude with Remote Control: a full claude.ai login, not the setup-token
wrangl controller secret set claude_credentials < ~/.claude/.credentials.json
wrangl template create -f docs/wrangl/templates/claude-interactive.yaml
wrangl project edit myapp --template claude-session   # or create a test project
wrangl fleet launch --template claude-session
wrangl task queue <sqid>
wrangl run attach <run>      # watch the PTY: were the first-run prompts answered?
```

What to check, and what to change if it's wrong:
- **First-run prompts.** The Claude template pre-accepts the trust and bypass prompts in
  Claude's own settings files (`merge: json`). If a session still sits at a prompt, copy its
  text into an `expect` rule's `match` (a regexp), with the keys it needs in `send` (`"\r"`
  is Enter). Then run `template edit`.
- **Remote Control.** The template starts Claude with `--remote-control "{{.SessionName}}"`,
  so the session appears in the Claude app as `<runner>:<task>`.
- **Chaining.** `wrangl logs <run>` shows `==> <sqid> … (same session)` each time the Stop
  hook hands over the next task, and `ending the interactive session` at the end.
- **Shared login.** Several runners refreshing one claude.ai login may log each other out.
  Keep `max_runners: 1` until you've seen two runs in a row succeed.

Template fields used here:
- `drive: interactive` runs one session per assignment.
- `secret_files` writes a controller secret to a file: 0600, owned by `agent`, deleted when
  the run ends.
- `expect` holds PTY prompt answers.
- `{{.Home}}` is the agent user's home.

## 9. The first project and run

```sh
wrangl project create web --template go-claude
wrangl repo link web your-org/your-repo            # the account's App must be installed on it
wrangl issue sync web && wrangl issue list --project web

# a task with two subtasks; the second waits for the first
wrangl task create web --title "Add a health endpoint" --prompt "Add GET /healthz returning 200."
wrangl task tree web                                # note the sqid, e.g. k3Xp9a
wrangl task split k3Xp9a --title "Write the handler and a test" --title "Document it in README" --chain
wrangl task queue k3Xp9a

wrangl run list --state active
wrangl run attach <run-id>            # live terminal, replayed from the start
wrangl logs <run-id> -f --stream agent
wrangl run diff <run-id> --stat       # live from the runner's workspace
```

When the run finishes, the branch `wrangl/<sqid>-<attempt>` is on GitHub with one commit per
subtask (`[sqid] title`), and the controller opens a PR (as the account's App). The subtasks
show ◐ (in review) until it merges. What happens next:

1. **Gates:** CI green (or no CI), changed lines ≤ the project's limit, no protected paths
   (`.github/**`, migrations, `go.mod`, `*.tf`, `Dockerfile*`).
2. **Review:** a review run on a runner reads the PR and calls `submit_review` with a risk.
3. **Decision:** with `--auto-merge` on the project, a low-risk PR that passes every gate is
   approved and squash-merged by wrangl (as the reviewer App if you made one, otherwise the account's App). Anything else becomes a question:
   ```sh
   wrangl question list                    # kind pr_review
   wrangl question answer <id> approve
   wrangl question answer <id> request_changes --text "rename the handler"   # queues a revise run
   wrangl pr list && wrangl pr show <id>   # or: wrangl pr approve|request-changes|close <id>
   ```
4. **Merged:** the subtasks become ✓ done, and blocked work waiting on them is queued again
   automatically.

CI results and merges done on GitHub arrive by webhook (step 10.4), with a reconcile poll every
15 minutes as a backstop.

`wrangl logs search 'panic OR timeout' --project web` searches every run's output.

## 10. Cloudflare Zero Trust, and the PWA

This puts the controller's API on a hostname you can reach from anywhere, behind Cloudflare
Access, with no inbound ports.

1. **Install cloudflared on the controller** and create a tunnel:
   ```sh
   sudo dnf install -y https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-aarch64.rpm
   cloudflared tunnel login
   cloudflared tunnel create wrangl
   cloudflared tunnel route dns wrangl wrangl.example.com
   ```
   `/etc/cloudflared/config.yml`:
   ```yaml
   tunnel: wrangl
   credentials-file: /root/.cloudflared/<tunnel-id>.json
   ingress:
     - hostname: wrangl.example.com
       service: http://127.0.0.1:8080
     - service: http_status:404
   ```
   Then `sudo cloudflared service install && sudo systemctl enable --now cloudflared`.
2. **Access application.** Zero Trust → Access → Applications → Add → **Self-hosted**,
   domain `wrangl.example.com`. Add a policy allowing only your email, and set a long session
   duration (an installed PWA re-authenticates awkwardly).
3. **Service token for the CLI.** Access → Service Auth → Create service token. Add an
   Access policy with action **Service Auth** that includes that token. Then on your laptop
   put this in `~/.config/wrangl/config.yaml`:
   ```yaml
   controller: https://wrangl.example.com
   cf_service_token: { id: "<client id>.access", secret: "<client secret>" }
   ```
   **Check:** `wrangl fleet list` from the laptop.
4. **Webhook bypass (M6).** Add a second Access application for
   `wrangl.example.com/gh/webhook` with a **Bypass** policy (everyone). GitHub authenticates
   with the HMAC secret GitHub generated for the App (step 6.2) instead.

5. **Let the controller verify Access itself.** Copy the application's **AUD tag**
   (Access → Applications → wrangl → Overview). Then in the controller config:
   ```yaml
   cloudflare:
     team: yourteam            # yourteam.cloudflareaccess.com
     aud: "<AUD tag>"
     owner_email: you@example.com
   ```
   Restart the controller. Every TCP request except `/gh/webhook` (HMAC-verified) and
   `/healthz` must now carry a valid `Cf-Access-Jwt-Assertion` for that AUD, issued to
   `owner_email` or to a service token. The unix socket is unaffected. `owner_email` also
   becomes the VAPID contact for web push.
   **Check:** `curl -i http://127.0.0.1:8080/backlog` on the controller returns 401, and the
   same page loads through the tunnel.

### 10.1 Install the PWA on your phone

1. Open `https://wrangl.example.com/` on the phone and sign in to Access. The page lands on
   the Backlog.
2. **iOS (16.4+):** Share → **Add to Home Screen**, then open wrangl from the Home Screen icon.
   Web push only works from the installed app. **Android/Chrome:** menu → **Install app**.
3. In the app: **Settings → Enable push on this device**, and allow notifications. The VAPID key
   pair is generated once at `<data_dir>/secrets/vapid.json`. Back it up with the rest of
   `secrets/`, because a new key invalidates every subscription.
4. **Check:** push arrives when a question is asked (for example, plan a task with
   **Split → Plan with agent**). Tapping it opens Inbox.

What the PWA does: **Backlog** (drag a task by its ⠿ handle to reorder or re-parent it; ＋ New;
open a task to edit, split, add subtasks or precursors, queue, or **Plan with agent**).
**Runners** (cards with CPU/mem/disk and a CPU sparkline; detail page with cordon, drain and
terminate). **Runs** (log filter, read-only live terminal, diff, file browser with markdown
preview). **Inbox** (agent questions, plan proposals with Accept / Edit / Reject, and PR
reviews). **Settings** (text size, projects, templates, launching a runner, push).

If a session expires, the installed app shows Cloudflare's login page. Sign in again; a long
Access session duration (step 2) keeps this rare.

### 10.2 The work stack from your own agents (remote MCP)

The controller serves the work-stack tools over MCP at `/mcp`. The tools let an agent list
projects, create, edit, move and split tasks, add precursors, complete or cancel tasks, queue
a task, and list runs. Your own Claude Code, on your laptop or on the controller, can then
plan and manage the backlog.

**Easiest: the stdio bridge.** It works wherever the `wrangl` CLI already reaches the
controller, using the service token from step 3:

```sh
claude mcp add wrangl -- wrangl mcp
```

**Direct HTTP** (without the CLI installed), with the same Access service token:

```sh
claude mcp add --transport http wrangl https://wrangl.example.com/mcp \
  --header "CF-Access-Client-Id: <client id>.access" \
  --header "CF-Access-Client-Secret: <client secret>"
```

**Check:** in Claude Code, `/mcp` lists `wrangl` with its tools. Ask it to "list my wrangl
projects".

Changes made this way are recorded as yours (`user` in task history). Cancels and deletes
need no approval, because you are the approver.

### 10.3 Task types, planning and deliverables

A task has a type that decides how its work is prompted, delivered and reviewed:

| Type | Deliverable | Review |
|---|---|---|
| `code` | pull request | gates, AI review, auto-merge if the project allows |
| `bug` | pull request with a failing-then-passing test | as `code` |
| `ux` | mockup images and a prototype link (`submit_deliverable`) | always the owner, visually |
| `architecture` | an ADR in the repo | always the owner reads it |
| `docs` | pull request limited to documentation | as `code` |
| `research` | a markdown report with sources (notes repo, or a `report_md` deliverable) | as `code`; read the sources |
| `review` | findings, no diff | the owner reads |
| `release` > `milestone` > `feature` | none: containers with a target date, a progress bar and an at-risk / late flag | a release waits for "Ship it" |

Runs queued on a planning task work through its subtree. Deliverables show on the task page and are kept for 90 days. When a run completes tasks but pushes nothing, the owner approves or requests changes from the inbox.

Runner templates for these live in `docs/wrangl/templates/`: `design.yaml` (Playwright Chromium for screenshots) and `research.yaml` (pandoc, pdftotext). Both are verified on arm64 Amazon Linux runners.

### 10.4 The runner terminal and the controller host

Runners view -> Terminal opens `/runner/NAME/term`: one tab per agent session, plus "+ Shell" (fish if installed, else bash; the first shell installs bat, lsd, dust, duf, fd, ripgrep and tldr through mise). Run tabs are watch-only until you tap "Take control"; shell tabs open in control.

To get a terminal on the controller machine itself, enrol it as a permanent runner (create a template with `arch` set to the host's, e.g. `docs/wrangl/templates/host.yaml`):

```
wrangl fleet enroll-token --template host
wrangl runner bootstrap --host --controller http://<private-ip>:4223 --token wre_...
```

It runs as a systemd `--user` unit under your own account, is pinned first in the Runners view, is never launched, reaped or powered off by the fleet, and leaves your own mise config alone.

## 11. First-run checklist (the unverified parts)

Watch these the first time and report anything odd:

| Step | Where to look | What to expect |
|---|---|---|
| `fleet launch` | `journalctl -u wrangl-controller`, EC2 console | instance running, tagged `wrangl:managed=true`, `wrangl:controller=wr-…`, `wrangl:runner`, `wrangl:template`; volume and ENI tagged too |
| IAM policy | controller log on launch/terminate | no `UnauthorizedOperation`; if you see one, the message names the missing action |
| Enrolment | `fleet list` (enrolling → provisioning) | if stuck in `launching`: `ssm connect` to the runner, read `/var/log/cloud-init-output.log` |
| `mise install` | `fleet show <runner>` template log | tools installed under `/home/agent/.local/share/mise` |
| `repo link` | CLI output | base branch detected; otherwise the account's App isn't installed on that repo |
| Claude Code | `wrangl run attach <id>` | the agent works in the repo; `exited N` failures show its output in `wrangl logs <id>` |
| Idle shutdown | `fleet list` 30+ min after the last run | the runner disappears; the EC2 console shows it terminated |
| Remote MCP (10.2) | `/mcp` in Claude Code | `wrangl` connected; with direct HTTP, a 401/403 means the service-token headers or the Access policy |
| Interactive / Codex templates (8.1) | `wrangl run attach <id>`, `wrangl logs <id>` | prompts answered, `(same session)` hand-overs, the session ended at the end |

## 12. Day 2: backups, upgrades, teardown

### Backups

`wrangl controller backup <path>` writes a consistent copy of the database while the
controller runs. A nightly timer to S3 (give the controller role `s3:PutObject` on the bucket):

```sh
sudo tee /etc/systemd/system/wrangl-backup.service >/dev/null <<'EOF'
[Service]
Type=oneshot
User=wrangl
ExecStart=/bin/sh -c 'f=/var/lib/wrangl/backup-$(date +%%F).db; rm -f $f; /usr/local/bin/wrangl --config /etc/wrangl/controller.yaml controller backup $f && aws s3 cp $f s3://YOUR-BUCKET/wrangl/ && rm -f $f'
EOF
sudo tee /etc/systemd/system/wrangl-backup.timer >/dev/null <<'EOF'
[Timer]
OnCalendar=daily
Persistent=true
[Install]
WantedBy=timers.target
EOF
sudo systemctl daemon-reload && sudo systemctl enable --now wrangl-backup.timer
```

Also back up `/var/lib/wrangl/nats/*.nk`, `nats/ca*.pem` and `secrets/`. Without the NATS
keys, every enrolled runner's credentials stop working.

### Upgrades

1. Copy the new binaries.
2. `sudo install` them to `/usr/local/bin/wrangl` and `/var/lib/wrangl/bin/`.
3. `sudo systemctl restart wrangl-controller`.

Migrations run on start. Runners keep running through the restart and reconnect. Newly
launched runners download the new binary.

**Existing runners upgrade themselves.** Each heartbeat carries the sha256 of the runner's
own binary. When it doesn't match the binary the controller serves for that arch:
1. The controller marks the runner. A marked runner takes no new runs, and `fleet list`
   shows it as `(upgrading)`.
2. Once the runner is idle (a run in progress finishes first), the controller sends it the
   expected hash.
3. The runner downloads `/dl/wrangl-linux-<arch>` from the same controller URL it enrolled
   with, checks the hash, and atomically replaces `/usr/local/bin/wrangl`. It then re-execs
   itself under systemd, with the same pid.
4. Its next heartbeat clears the mark.

A failed download or checksum leaves the old binary in place. The controller retries every
2 minutes. **Check:** `wrangl fleet show <runner>` shows `wrangl binary sha256 …`, and the
controller log says `runner upgraded`.

### Teardown

```sh
wrangl fleet teardown                         # on the controller: this controller's runners
wrangl fleet teardown --region us-west-2 --controller-id wr-xxxxxxxxxx   # from a laptop
wrangl fleet teardown --region us-west-2 --all-controllers --yes
```

Teardown talks to AWS directly, so it works even if the controller is gone. It never touches
untagged instances, and root volumes are deleted with their instances. Two safety nets cover
the case where nobody runs teardown: an orphaned runner powers itself off after 2 hours
without its controller, and the shutdown terminates the instance. Then delete the controller
instance, the instance profiles and roles, the SG and the NAT gateway if you made one.

## 13. Troubleshooting

| Symptom | Likely cause |
|---|---|
| `fleet launch`: "listen.enroll must be the controller's private VPC IP" | `listen.nats` / `listen.enroll` still on 127.0.0.1 (step 5.4) |
| `fleet launch`: "no aarch64 runner binary" | copy `wrangl-linux-aarch64` into `/var/lib/wrangl/bin/` |
| Runner stuck `launching` | SG doesn't allow 4222–4223 from itself; subnet has no NAT (can't reach dnf/GitHub); look at cloud-init output on the runner |
| Enrol rejected "request from X, but instance has private IP Y" | something between runner and controller rewrites the source IP (NAT, proxy, different VPC) |
| Runner `idle (cordoned)` with "last error" | template apply failed; `wrangl fleet show <runner>` has the output tail. Fix with `wrangl template edit` |
| `repo link`: "GitHub is not configured" / "not installed" | no App for the repo's owner (add the account on the Setup page) or the App isn't installed on that repo |
| Run `failed`, "agent exited N" | `wrangl logs <run>` and `wrangl run attach <run>` show what the agent printed |
| Run `blocked` | remaining subtasks wait on tasks outside the run; finish those, then `wrangl task queue` again |
| CLI from laptop: HTTP 403 from Cloudflare | the service-token policy is missing, or the `cf_service_token` id/secret are wrong |

Useful on any runner (`ssm connect <region>/wrangl-<runner>`):
- `sudo wrangl --config /etc/wrangl/runner.yaml runner status`
- `journalctl -u wrangl-runner`

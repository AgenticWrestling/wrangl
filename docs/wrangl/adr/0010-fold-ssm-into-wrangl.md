# ADR 0010: Fold the useful parts of `ssm` into `wrangl`, with mise for software

- Status: **Proposed** (the owner reads and decides before it merges)
- Task: sE5mgv, under "Fold the useful parts of ssm into wrangl (mise-based), then remove ssm" (bzXHd1)
- Builds on: [ADR 0002](0002-remote-runners.md) (SSM port-forwarding, EC2 Instance Connect as a credential option), runner templates (`mise_toml`, `setup_script`), `runner.Applier`, `wrangl runner bootstrap`
- Touches: a new `wrangl dev` command group, a new shared AWS package, `runner/remote.go` (tunnel supervisor moves out), `runner/apply.go` and `runner/term.go` (one new exported function and one exported constant), `Taskfile.yml`, README, INSTALL.md, aws-iam.md, `go.mod`
- Numbering note: `adr/0010-question-batches.md` already exists and the task fixed this file name, so there are two 0010s. Renumber one (suggest this one, to 0011) before merge; nothing links to either yet.

## Context

`cmd/ssm` is a separate 2.3k-line Go binary (`ssm connect | tunnel | ssh | setup | start`) built by
`task build` / `task install` into `~/bin/ssm`. It was written first (spec
`docs/superpowers/specs/2026-06-26-ssm-tool-design.md`, plans m1 to m5) and wrangl later grew its own
AWS code around it. The two now overlap, and the owner's goal is one tool: keep what is worth keeping
inside `wrangl`, delete `ssm`, and install software on machines with **mise**, not shell scripts.

What is true today, verified in the code:

**ssm** (`cmd/ssm`)
- `main.go` has five commands. Every one repeats the same front end: pick a profile with a fuzzy
  finder (`ui.PickProfile`, MRU order from `~/.config/ssm/history.json`), refresh an expired SSO
  token by exec-ing `aws sso login`, then resolve `region/name` by the `Name` tag (`resolveTarget`;
  `startCmd` has its own copy of the same profile and SSO block). With no region it falls back to a
  hard-coded `us-east-1`, ignoring the profile's configured region (`ProfileInfo.Region` is read for
  display only).
- `connect` and `tunnel` exec `aws ssm start-session` (the CLI plus the Session Manager plugin do the
  websocket). `tunnel` waits for the local port, prints a URL, and `--open` runs macOS `open`.
- `ssh` pushes an Ed25519 public key with `ec2instanceconnect.SendSSHPublicKey` (valid 60 s),
  upserts a `Host` block (`ProxyCommand aws ec2-instance-connect open-tunnel`, comments `# ssm-region:` and
  `# ssm-profile:` carry the metadata), then execs `ssh`. The key is `~/.ssh/id_ed25519`, the user's
  own default key. **The Host block does not push a key**, although the spec says the ProxyCommand
  "handles this automatically": `open-tunnel` only tunnels, so a plain `ssh NAME` or `scp` works for
  60 s after the last `ssm ssh` and then fails.
- `setup` creates an EC2 Instance Connect Endpoint in the VPC if absent (`eic.EnsureEndpoint`), adds
  an SG rule for TCP 22 from the VPC CIDR, **opens all protocols and ports from the caller's public IP**
  on the instance's security groups (`vpc.EnsureMyIPIngress`, rule description `ssm-managed`), pushes
  the key, writes the Host block, then sends one 230-line bash script by `ssm:SendCommand`
  (`AWS-RunShellScript`, 1 h timeout) and tails its CloudWatch log stream (`cwlogs.Tail`).
- The script installs: AWS CLI v2, `ec2-instance-connect` (dnf), fish (GitHub release, made the login
  shell), Go (dnf), Rust (rustup), the Zed remote server, a personal user `njr`, Java 25 Corretto,
  the Android cmdline tools, SDK platforms and a licence acceptance, the Android CLI (hard-coded
  `linux_x86_64`), Python Playwright with Chromium and its libraries, and a systemd timer that stops
  the instance when idle. The timer's labels say 4 h but `IDLE_THRESHOLD_SECONDS=$((1 * 3600))`
  is one hour, and it stops the instance with `aws ec2 stop-instances` from a root systemd unit
  whose `PATH` has no mise shims. It re-runs everything on every invocation (no idempotency marker).
- `start` lists `stopped`/`stopping` instances and `ec2:StartInstances` plus a 5 minute waiter.

**wrangl** (`cmd/wrangl/internal`)
- There is **no** AWS profile, region, SSO or interactive-selection code. The AWS config is loaded in
  three places: `cli/fleet.go` (`TeardownEC2`, with `--region/--profile`), `controller/server.go`
  (instance role) and, in ssm, `aws.LoadConfig`.
- AWS usage is on the controller (`controller/provision`: `EC2API`/`SSMAPI` interfaces with fakes in
  `provision/fake.go`, `Launch`, `Describe`, `Terminate`, `ListManaged`, `Teardown`; `controller/costs`).
  `provision.Instance` carries ID, type, AZ, private IP, state and tags only (no VPC, subnet, security
  groups or Name), so it cannot replace ssm's `Instance`.
- `runner/remote.go` (`realTargets.OpenTunnel`) already supervises an `aws ssm start-session ...
  AWS-StartPortForwardingSession` per remote target: reference counted, restarted with backoff, ready
  probe of 45 s. It is the better tunnel code (ssm's has no restart), but it is private to the runner
  package and takes a `proto.TargetConn`. ADR 0002 Q2-A chose this mechanism "the project already uses
  for `cmd/ssm`".
- Nothing in wrangl uses EC2 Instance Connect, CloudWatch Logs or `golang.org/x/crypto/ssh`; those SDK
  modules and `go-fuzzyfinder`, `ini.v1` are in `go.mod` only for ssm (checked with grep over
  `cmd/wrangl`).
- Software installation is already mise-based: a template has `mise_toml` (written to the agent's
  `~/.config/mise/config.toml`) and `setup_script` (bash, as root, hash-gated); `runner.Applier.Apply`
  installs mise (`DefaultMiseInstaller`), runs the script, writes the config and runs `mise install`
  as the agent user; the first web-terminal shell installs `bat lsd dust duf fd ripgrep tealdeer`
  through `conf.d/wrangl-shell.toml` (`runner/term.go` `shellTools`); the host runner is deliberately
  left unapplied. Provisioning `userdata.go` installs only git and fish (as OS packages) before
  `wrangl runner bootstrap`.
- `wrangl runner bootstrap` enrols a box with a controller. A user's own dev instance does not need
  that (no NATS, no controller, no scheduling), so it is the wrong mechanism for them.
- The runner web terminal (`/runner/NAME/term`, `Worker.OpenShell`) already gives a shell on any
  runner; it does not cover the controller instance or a dev instance.
- `INSTALL.md` (steps 5.1 and 5.2, troubleshooting) and `aws-iam.md` tell the reader to use
  `ssm connect` and `ssm setup` to reach the controller and runners, so ssm is already a documented
  dependency of installing wrangl, and it must keep working before a controller exists.

## Options

### Q1. Which capabilities live in wrangl, and where?

**A. Keep ssm as is and only share a library.** Move shared helpers to a package both binaries import.
- Pros: least change. Cons: two binaries to build, install and document; the goal says remove ssm.
  Rejected.

**B. Everything in, one-to-one.** Port all five commands and the whole script.
- Pros: no behaviour lost. Cons: carries the unsafe all-ports-from-my-IP rule, the personal `njr` user,
  the hard-coded Android and Zed installs, and the bash toolchain script that contradicts the
  mise principle. Rejected.

**C. Merge the access and lifecycle capabilities into a new `wrangl dev` group, replace the setup script
with a mise apply, drop what is unsafe or personal.** (Chosen; the detail is below.)

### Q2. Which name and group?

**A. `wrangl dev ...`** ("your own dev instances", separate from `fleet`, which is the controller's
runners). Works without a controller, like `fleet teardown`.
**B. Extend `wrangl fleet`.** `fleet` is the controller-managed runner lifecycle, and its commands
need the API (except `teardown`). Putting profile pickers and EIC there blurs "controller-owned" with
"yours".
**C. Top-level `wrangl connect|tunnel|ssh|setup`.** Short, but pollutes the root group list and
`setup` collides in meaning with the controller's Setup page.

Decision A, plus thin `wrangl fleet connect|tunnel RUNNER` that resolve the runner's instance id
through the controller API and call the same code (so the docs can say `wrangl fleet connect NAME`).

### Q3. How does a wrangl command apply a mise config to a machine it can reach over SSM or SSH?

**A. Port ssm's bash script into wrangl as a Go string.** Same shell, same drift. Rejected by the
principle: it hard-codes versions and URLs, is not idempotent and cannot be edited by the owner.

**B. Render a small apply script from the same inputs a template has, and run it through an
`Executor`.** The input is `proto.TemplateApply`-shaped (`mise_toml`, `setup_script`). A new function
`runner.ApplyScript(t, user)` renders the four steps `Applier.Apply` performs (install mise to
`/usr/local/bin/mise` if missing via mise.run, run the setup script when its hash changed, write the
mise config, `mise install --yes` as the user). Transports:
  - `ssm`: `ssm:SendCommand` (`AWS-RunShellScript`), output streamed from CloudWatch Logs
    (`cwlogs`) with a fallback to polling `GetCommandInvocation` (output is truncated at 24,000
    characters there, so CloudWatch stays the main path). No inbound port, no SG change.
  - `ssh`: `ssh HOST 'sudo bash -s' < script` using the system `ssh`, so it inherits the `Host` block
    (EIC ProxyCommand) and also works for non-AWS machines (a Mac mini, a home server).
  - Pros: works with nothing preinstalled but `curl` and a root path; one code path for any machine;
    the output streams live; the owner edits a TOML file.
  - Cons: the four steps now exist twice (Go in `Applier`, shell in `ApplyScript`). Mitigated by a
    parity test that runs both against `runnertest.FakeMise` in a temp root and compares the
    observable result (config file content, `install` called as the user, hash skip), and by
    leaving `Applier` unchanged.

**C. Put the wrangl binary on the machine and run a hidden `wrangl machine apply` that calls
`Applier.Apply`.** One implementation. But the binary has to get there: SSM cannot upload files
(`send-command` parameters are small), so the machine must download it (the GitHub release works
only for tagged builds, and the controller's `/dl/` only for machines that can reach the controller).
It fails for a dev build and adds a version-skew question. Kept as a possible later optimisation, not
the base.

**D. Enrol the dev instance as a runner and let the controller apply the template** (heartbeat
driven). Reuses everything. But it needs the NATS ports open to the controller, makes the box
claimable by the scheduler, counts against `max_runners`, and the daemon owns `config.toml`. The
`host` template already shows the nuance (applied nothing). Wrong for personal instances.

Decision B.

### Q4. Where does the OS-level residue go?

**A. A second embedded shell script in Go (like ssm's).** **B. The profile's `setup_script`,** the field
templates already have: root bash, hash-gated, run before mise. The default profile ships a short one.
Decision B, so there is no special "provision" concept: a dev profile is a `{mise_toml, setup_script}` pair.

### Q5. Where does shared AWS code live and whose helpers win?

**A. Extend `controller/provision`.** It is controller-side, and importing it into a CLI that must run
without the controller pulls in the launch lifecycle. **B. A new `cmd/wrangl/internal/awsx` package**
that the CLI, the runner and later `provision` import. Decision B (details under "Duplicated AWS code").

## Decision

Adopt Q1-C, Q2-A, Q3-B, Q4-B, Q5-B.

### 1. Inventory and decisions

| ssm capability | Decision | Where in wrangl, and what it reuses |
|---|---|---|
| `connect`: SSM session | **Merge** | `wrangl dev connect [TARGET]` (`cli/dev.go`), exec `aws ssm start-session` as ssm does, with `awsx.Resolve` for profile, region and instance. Also `wrangl fleet connect RUNNER` (instance id from the runner view's `InstanceID`). Partly covered: the runner web terminal (`Worker.OpenShell`) already opens a shell on a runner, but not on the controller or a dev instance. |
| `tunnel`: SSM port-forward | **Merge, wrangl's supervisor wins** | `wrangl dev tunnel [TARGET] RPORT[:LPORT] [--open]`. Extract the supervised `aws ssm start-session` loop and ready probe from `runner/remote.go` (`realTargets.OpenTunnel`) into `awsx/ssmtunnel`, called by both `runner/remote.go` and `dev tunnel`; `ParsePortSpec` and `--open` move over (`--open` uses `xdg-open` on Linux; ssm's `open` is macOS-only). Foreground and one-shot: Ctrl-C closes. Replaces ssm's `waitForPort` and one-shot `proc`. |
| `ssh` via EC2 Instance Connect key push | **Merge** | `wrangl dev ssh [TARGET] [-- ARGS]` and a hidden `wrangl dev proxy HOST` (the Host block's `ProxyCommand`: push the key, then `aws ec2-instance-connect open-tunnel`), which fixes the "works for 60 s" defect. `awsx/eic.PushPublicKey`. Uses a dedicated `~/.ssh/wrangl_dev_ed25519` key instead of the user's default `id_ed25519` (ssm reuses and may create it). ADR 0002 Q3-B named EIC as the way to get a short-lived key for Linux targets; this package is what that future work will import. Not covered in wrangl today (checked: no `ec2instanceconnect` use outside ssm). |
| EIC endpoint creation and the SG rule for port 22 from the VPC CIDR | **Merge, narrowly** | `awsx/eic.EnsureEndpoint` and `awsx/sg.EnsureSSHFromVPC`, run by `wrangl dev ssh --prepare-network` (and by `dev setup --via ssh`), never implicitly. They are what the EIC path needs when the instance has no public address; an endpoint is shared, billed infrastructure in the VPC, so the user asks for it. |
| `vpc.EnsureMyIPIngress` (all ports from the caller's public IP, `ssm-managed`) | **Drop** | Opens every port to one IP, contradicts the "no inbound ports" stance of ADR 0002 (decision 3), INSTALL.md and `aws-iam.md`, and neither SSM nor EIC needs it. The replacement is `dev tunnel`. Existing `ssm-managed` rules stay in AWS; `dev setup` prints a warning naming any SG it finds with one, and `dev setup --revoke-legacy-rules` removes them (small, optional task). |
| `setup`: provision toolchain (bash) | **Replace** | `wrangl dev setup [TARGET]` applying a mise profile; see section 3. The AWS CLI, Go, Rust, Java, Node, Python and the CLI tools become mise tools; OS packages stay in a short `setup_script`. |
| `setup` phase 3: CloudWatch Logs tail | **Merge** | `awsx/cwlogs` as the output stream of `awsx/ssmrun` (SendCommand + stream + wait), generalised: the text "Provisioning complete" becomes the caller's. Not used by wrangl today (checked). |
| `start`: start a stopped instance | **Merge** | `wrangl dev start [TARGET]`, plus `dev stop [TARGET]` (new, a few lines, and the idle-stop story needs it) and `dev list [--state running|stopped|all]`. `awsx.StartInstance` (StartInstances plus the 5 minute waiter). Not for runners: the fleet launches and terminates, it never stops. |
| Instance picker, profile and region resolution, MRU history | **Merge, fixed** | `awsx/profiles.go`, `awsx/sso.go`, `awsx/history.go`, `awsx/resolve.go` (ssm's code is the only implementation). One `awsx.Resolve(ctx, Target)` replaces the two copies in `main.go`. Region order: `--region`, `region/` prefix, `AWS_REGION`, the profile's `region`, then ask. No hard-coded `us-east-1`. History at `~/.config/wrangl/dev-history.json` (next to `config.yaml`, `config.DefaultClientPath`); the old `~/.config/ssm/history.json` is read once if the new one is missing. `AWS_PROFILE` and `--profile` skip the picker, which also lets scripts run it non-interactively. |
| SSO expiry detection and `aws sso login` refresh | **Merge** | `awsx/sso.go` (`SSOTokenExpired`, `isSSOExpired`, `RefreshSSO`), also used by `fleet teardown`'s AWS config (which today fails with the raw SDK error). |
| VPC discovery (`DescribeVpcs` for the CIDR) | **Merge with the SG rule** | Only exists to compute the SG rule's CIDR; moves with `awsx/sg`. There is no separate "discovery" feature. |
| `~/.ssh/config` generation and lookup | **Merge** | `devbox/sshconfig.go` (`UpsertHostBlock`, `LookupHost`, preserving unmanaged lines, which is tested today). Markers become `# wrangl-region:`, `# wrangl-profile:`; the old `# ssm-*` markers are read as a fallback for one release. `wrangl dev ssh-config [TARGET]` writes the block without connecting. |
| Ed25519 key generation | **Merge** | `devbox/keygen.go` using `golang.org/x/crypto/ssh`, for the dedicated key above. |
| Spinner | **Drop** | The commands now stream real progress lines (apply output, EIC waits); a goroutine spinner on stderr fights with streamed output. Plain lines, with elapsed time when waiting. |
| Fuzzy picker | **Merge** | `awsx/pick.go`, `go-fuzzyfinder` as today. wrangl's `tui` package uses bubbletea, but a list picker needs ~80 lines either way; revisit only if the binary-size check (risk 2) fails. |
| Script item: AWS CLI v2 | **mise** | `aws-cli = "latest"` (registry: `aqua:aws/aws-cli`). |
| Script item: `ec2-instance-connect` (dnf) | **OS step** | In the default `setup_script` (idempotent `dnf install`/`apt-get install`), needed for the EIC SSH path. |
| Script item: fish and `chsh` | **mise, login shell opt-in** | fish is not in mise's registry (checked with `mise registry`), so `"github:fish-shell/fish-shell" = "latest"` in an optional overlay; making it the login shell is a personal choice, so `dev setup --shell fish` does it (and writes `/etc/shells`); not the default. |
| Script item: Go (dnf), Rust (rustup), Java 25 Corretto | **mise** | `go`, `rust`, `java = "corretto-25"` (registry has `core:go`, `core:rust`, `core:java`). |
| Script item: Zed remote server | **Drop** | Zed installs its remote server over SSH when it is missing. (Verify against the current Zed docs in the task; if it does not, add a `github:` tool to an overlay.) |
| Script item: user `njr` | **Drop (as default)** | A personal user. `--user NAME` selects who the tools are installed for (default `ec2-user`, or the SSH user); `--create-user` creates it in the OS step if wanted. |
| Script item: Android SDK and CLI, JDK | **Overlay** | `dev setup --with android`: an embedded overlay with `android-sdk` (registry: `vfox:jdx/vfox-android-sdk`) and its `setup_script` for licences and `sdkmanager` packages. The hard-coded `linux_x86_64` download goes. Related: [android.md](../android.md). |
| Script item: Playwright and Chromium libraries | **Overlay** | `--with browser`: `npm:playwright` plus the OS library list in its `setup_script` (dnf). |
| Script item: idle auto-stop timer | **Merge, simplified, opt-in** | `dev setup --idle-stop 4h` installs a systemd timer whose service ends with `systemctl poweroff` (instance-initiated shutdown stops an EBS-backed instance by default; the runner's own `PoweroffCommand` is the same). No AWS CLI, no instance-profile permission, no PATH problem, and the interval actually matches the flag (ssm labels 4 h but uses 1 h). Lives in `devbox/idlestop.go` as an embedded unit, applied through `setup_script`. |

### 2. Target command surface

```
wrangl dev list      [--profile P] [--region R] [--state running|stopped|all]
wrangl dev connect   [TARGET]
wrangl dev ssh       [TARGET] [-- ssh args or a remote command]  [--prepare-network]
wrangl dev ssh-config [TARGET]
wrangl dev tunnel    [TARGET] RPORT[:LPORT] [--open]
wrangl dev start     [TARGET]
wrangl dev stop      [TARGET]
wrangl dev setup     [TARGET] [--template NAME | --file FILE] [--with android,browser]
                     [--via ssm|ssh] [--user U] [--idle-stop 4h] [--shell fish] [--dry-run]
wrangl fleet connect RUNNER      # thin: runner -> instance id from the API, then dev connect
wrangl fleet tunnel  RUNNER RPORT[:LPORT]
(hidden)  wrangl dev proxy HOST   # the ssh ProxyCommand
```

`TARGET` is `[region/][user@]name`, an instance id (`i-...`), or an existing `Host` alias from
`~/.ssh/config`; absent, the picker runs. Global flags `--profile`, `--region` (and `AWS_PROFILE`,
`AWS_REGION`) apply. None of the `dev` commands needs a controller or `~/.config/wrangl/config.yaml`
(only `--template` and `fleet connect` do), so they work at INSTALL.md step 5, before the controller
exists. `dev` appears in `cmd/wrangl/internal/agentplugin/assets/skills/wrangl/SKILL.md` (the skill test checks every named command exists).

### 3. How mise is used

**One apply pipeline for runners and your own machines.** A dev profile is exactly the two template
fields that install software:

```yaml
mise_toml: |
  [tools] ...
setup_script: |
  dnf install -y ...
```

`wrangl dev setup` builds a `proto.TemplateApply`-shaped value from one of:
1. `--template NAME`: `mise_toml` and `setup_script` of a runner template, fetched through the API
   (`wrangl template show`). So a dev instance can be made identical to a runner.
2. `--file FILE`: a YAML file with just those two keys (same decoder as `fleet.Template`).
3. Nothing: the embedded default profile below. `--with android,browser` appends embedded overlays
   (their `[tools]` merged, their `setup_script`s concatenated).

Then `runner.ApplyScript(apply, user)` renders a self-contained bash script, and an `Executor` runs
it as root. The script does what `Applier.Apply` does, in the same order:
1. install mise to `/usr/local/bin/mise` if missing (the same `curl https://mise.run` with
   `MISE_INSTALL_PATH` as `DefaultMiseInstaller`; no new trust assumption, still unchecksummed);
2. run `setup_script` only when its sha256 differs from `/var/lib/wrangl-dev/setup.sha256`;
3. write the config, then
4. `sudo -u USER -H` `mise install --yes`.

**Different from a runner on one point:** the config is written to `~/.config/mise/conf.d/wrangl-dev.toml`,
not `config.toml`. `config.toml` stays the user's own (and on a runner it is owned by the daemon's
`Applier`); `conf.d` is the same mechanism `runner/term.go` already uses for `wrangl-shell.toml`, so the
shell tools and a dev profile compose. If `/etc/wrangl/runner.yaml` exists, `dev setup` warns that the
machine is a runner and the template owns the toolchain. mise `latest` and `stable` are used where the
old script used "whatever dnf had"; a team that wants pins edits the file.

**Executors** (`awsx/ssmrun`, `devbox/sshexec`): `ssm` (`SendCommand`, CloudWatch stream, 1 h timeout
as today) and `ssh` (system `ssh HOST 'sudo bash -s'`; works for any SSH-reachable machine). Default
`--via ssm`; if the SSM agent is not online (`DescribeInstanceInformation`) the command says so and
suggests `--via ssh`. `--dry-run` prints the script.

**Default dev toolchain** (`cmd/wrangl/internal/devbox/default.yaml`, `go:embed`; tool names checked in
`mise registry` on 2026.11.4, versions resolved at apply time):

```toml
[tools]
aws-cli = "latest"
go      = "latest"
rust    = "stable"
node    = "lts"
python  = "latest"
uv      = "latest"
java    = "corretto-25"
# the CLI tools a runner's first shell installs (the shared runner.ShellTools constant)
bat = "latest"
lsd = "latest"
dust = "latest"
duf = "latest"
fd = "latest"
ripgrep = "latest"
tealdeer = "latest"
```

and the entire OS residue:

```sh
set -eu
pm=$(command -v dnf || command -v apt-get)
$pm install -y git tar unzip gzip ca-certificates ec2-instance-connect
```
(apt: package names differ slightly; the task owns that detail.) That is the "minimal setup step": no
language, no user, no shell, no AWS CLI. `runner.shellTools` becomes the exported `runner.ShellTools` so
the default profile and the first-shell prelude cannot diverge.

### 4. Duplicated AWS code: whose helpers win

| Concern | ssm | wrangl | Winner |
|---|---|---|---|
| Load AWS config for profile and region | `aws.LoadConfig` | `cli.TeardownEC2` (adds a "no region" error), `controller/server.go` (instance role, no profile) | One `awsx.LoadConfig(ctx, profile, region)` with wrangl's no-region error; `TeardownEC2` calls it. The controller's role-based load stays as is. |
| Profile listing, SSO, MRU history, picker | `aws/profiles.go`, `sso.go`, `history.go`, `ui` | none | ssm (only implementation); fixed as described. |
| Instance model and discovery | `aws.Instance` (ID, Name, VPC, subnet, SGs, IP), `ResolveInstance`, `listInstancesByState` on concrete clients | `provision.Instance` (no VPC, SGs, Name), `Describe`, `ListManaged` behind `EC2API` with fakes | Model: ssm's (superset). Test seam: wrangl's (narrow interfaces plus a fake, as `provision/fake.go`); ssm's tests only cover parsing today. `provision` may later convert to `awsx.Instance`; not required. |
| SSM port-forward | `buildTunnelArgs` (JSON `--parameters`), one-shot | `OpenTunnel` (`key=value` parameters, supervised, backoff, shared per target) | wrangl's, extracted to `awsx/ssmtunnel`. Both call the same `aws` CLI. |
| Waiting for a local port | `waitForPort` (15 s) | ready probe in `OpenTunnel` (45 s) | wrangl's, with a timeout argument. |
| Starting a session | `syscall.Exec` of `aws ssm start-session` | none for interactive use | ssm's. |
| EIC, CloudWatch Logs, SG helpers, ssh config and keys | only ssm | none | ssm's, moved. |

`go.mod` consequence: `service/ec2instanceconnect`, `service/cloudwatchlogs`, `go-fuzzyfinder`,
`ini.v1` and `x/crypto` stop being "ssm only" and are used by `awsx`/`devbox`; `service/ssm` was already
direct for `provision`. After the move, `go mod tidy` is expected to change nothing.

## Migration and removal

Stages (each ends with `go build ./...`, `go vet ./...`, `go test ./...` green and ssm still working,
until stage 5):

1. **Extract.** Create `awsx` and move/port, with tests, config, profiles, SSO, history, instance
   resolution, EIC, SG, cwlogs, ssmrun and `ssmtunnel`; point `runner/remote.go` and `TeardownEC2`
   at it. No new command yet. ssm is not edited.
2. **`wrangl dev` access commands:** `list`, `connect`, `ssh`, `ssh-config`, `proxy`, `tunnel`,
   `start`, `stop`, and `fleet connect|tunnel`.
3. **`wrangl dev setup`:** `runner.ApplyScript` with its parity test, executors, default profile,
   overlays, `--idle-stop`, `--shell`.
4. **Docs switch:** INSTALL.md, aws-iam.md, runners.md, ADR 0002 text, README, skill; `ssm` is no
   longer mentioned as a prerequisite (the AWS CLI and Session Manager plugin still are).
5. **Remove ssm** (the final task). It deletes:
   - `cmd/ssm/` entirely (`main.go`, `connect_test.go`, `tunnel_test.go` and `internal/{aws,cwlogs,eic,provision,ssh,tunnel,ui,vpc}`; 24 files).
   - `Taskfile.yml`: the `BINARY` and `INSTALL_PATH` vars and the `build` and `install` tasks, and the
     `default` task (it points at `build`; repoint it to `build-wrangl`). Check `task --list`.
   - `README.md` "Also in this repo: ssm" (lines 110 to 114).
   - `docs/superpowers/specs/2026-06-26-ssm-tool-design.md` and `docs/superpowers/plans/2026-06-26-ssm-m1-scaffold.md`, `m2-connect`, `m3-tunnel`, `m4-setup-eic`, `m5-setup-provision`.
   - `.superpowers/sdd/progress.md` (the "SSM Tool: SDD Progress Ledger"; the only tracked file in that directory).
   - references in `docs/wrangl/INSTALL.md` (lines 68, 155, 204, 209, 899, 1004), `docs/wrangl/aws-iam.md:109`, `docs/wrangl/guide/runners.md:287`, `docs/wrangl/adr/0002-remote-runners.md:81` (the sentence "the project already uses for `cmd/ssm`"; ADRs are history, so reword to name `wrangl dev tunnel`, not delete). `android.md`, `INSTALL.md:568` and `controller.sh` mention the `aws ssm` CLI, not the tool: leave them.
   - `go.mod`/`go.sum`: run `go mod tidy`; anything left that only `cmd/ssm` imported must go (expected none after stage 1, see above).
   - the legacy-compat reads (`# ssm-*` markers, `~/.config/ssm/history.json`), if they were scheduled for removal.
   - Then `grep -rniE '\bssm (connect|setup|tunnel|ssh|start)|cmd/ssm|build/ssm|bin/ssm'` must be empty (the `aws ssm` CLI and `service/ssm` SDK hits are expected). `provision.logs` (empty, tracked, repo root) is unrelated to the tool; leave it unless the owner says otherwise.
   - On user machines (not in the repo): delete `~/bin/ssm`; `~/.ssh/config` `Host` blocks written by ssm keep working (they call `aws` directly) until `wrangl dev ssh` rewrites them with the new proxy command.

## Consequences

Good:
- One binary, one install path (`install.sh`); `ssm` prerequisites disappear from INSTALL.md.
- Dev instances and runners share one software mechanism; a dev instance can be built from a
  runner template, and the owner edits TOML instead of a Go string.
- Fixes four ssm defects as a side effect: key expiry on plain `ssh`, hard-coded `us-east-1`, the
  idle-stop mislabel and the all-ports rule.
- The EIC and SG code is ready for ADR 0002's Q3-B (short-lived SSH keys for Linux remote targets).

Costs and risks:
1. **Two implementations of the apply steps** (Go `Applier`, rendered bash). Parity test; keep
   `Applier` untouched. A later option is Q3-C when releases make the binary available.
2. **Binary size.** The runner is this same binary and downloads it at boot and on every upgrade
   (Taskfile comment). `go-fuzzyfinder` brings tcell. Stage 1 measures `build-wrangl` before and after;
   if the growth is unreasonable, replace the picker with a bubbletea list (already linked) or put
   `dev` behind a build tag for the runner image.
3. **mise registry coverage.** `fish`, the Zed server and the Android SDK are not plain registry tools
   (fish: `github:` backend; android: vfox plugin). Their versions of asset naming and the `setup_script`
   for `sdkmanager` licences must be tested on a real instance; the `setup_script` is the escape hatch,
   so the default profile (no overlays) has no such tools. The tool names above were checked against
   `mise registry` here, not installed.
4. **`mise.run` is fetched without a checksum**, as for runners today. A pinned installer checksum
   would help both; out of scope.
5. **`latest` toolchains drift** between instances set up on different days; a profile can pin.
6. **SSM output.** Streaming needs the `/aws/ssm/AWS-RunShellScript` log group and instance-role
   `logs:` permissions (ssm already assumed this); `aws-iam.md` gets the policy. Without it the
   fallback shows only the last 24,000 characters.
7. **Existing users' `Host` blocks and keys.** Moving to `wrangl_dev_ed25519` means instances must
   accept the new key (each `dev ssh` pushes it, so this is automatic) but any `authorized_keys`
   entry written earlier is left alone.
8. **Not verified:** nothing here was run against a real AWS account; the AWS API behaviour (EIC,
   SendCommand, instance-initiated shutdown becoming a stop) is taken from ssm's working code and AWS
   documentation, and the implementation tasks should say what they smoke-tested.

Out of scope: launching dev instances (the user has their own), Windows, a web UI for `dev`, replacing
`deploy/wrangl/controller.sh`'s use of the `aws ssm` CLI.

## Task breakdown (code tasks for the implementation feature)

1. **awsx foundation.** New `cmd/wrangl/internal/awsx`: `LoadConfig`, profiles, SSO (check and `aws sso login`), MRU history with
   legacy read, `Instance`, `Resolve`/list/start/stop with narrow interfaces and fakes, tests ported from
   ssm. Replace `cli.TeardownEC2`'s config load. Record the binary size before and after.
2. **Extract `ssmtunnel`.** Move the supervised port-forward loop out of `runner/remote.go` into
   `awsx/ssmtunnel`; keep the `runner` tests green; add `ParsePortSpec`.
3. **`wrangl dev` access.** `cli/dev.go`: `list`, `connect`, `tunnel`, `start`, `stop`, `fleet connect|tunnel`; picker; `--profile/--region`; register in `root.go`; update `cmd/wrangl/internal/agentplugin/assets/skills/wrangl/SKILL.md`.
4. **EIC ssh.** `awsx/eic` (key push, endpoint, SG-from-VPC), `devbox` ssh config and keygen (legacy
   marker read), `dev ssh`, `dev ssh-config`, hidden `dev proxy`, `--prepare-network`; tests ported from ssm plus proxy tests with a fake.
5. **Apply script.** `runner.ApplyScript`, export `runner.ShellTools`, the parity test against `FakeMise`;
   `devbox` default profile and overlays (`go:embed`), `--file` and `--template` loading.
6. **`wrangl dev setup`.** Executors (`awsx/ssmrun` + `cwlogs`, ssh), `--via`, `--user`, `--dry-run`,
   `--idle-stop`, `--shell`, the runner-box warning and the legacy `ssm-managed` rule warning and `--revoke-legacy-rules`.
7. **Docs switch.** INSTALL.md steps 5.1/5.2 and troubleshooting, aws-iam.md (logs, SendCommand,
   StartSession, EIC), runners.md, ADR 0002 wording, README, DESIGN/DEVELOPMENT if needed, a new
   `docs/wrangl/guide/dev-instances.md` and a link from the docs index; add the profile example
   `docs/wrangl/templates/dev.yaml`.
8. **Smoke test on a real instance** (owner's account): connect, tunnel, ssh after 60 s, setup twice
   (second run skips), setup with `--with android`, idle-stop fires. Report results in the task.
9. **Remove ssm** (the stage 5 list above), last, with the grep check and the three Go commands.

## Implementation notes (as built)

Deviations from the plan above, found while building it:

- `dev setup --shell fish` was **not** built: `setup_script` runs before `mise install`, so it cannot point the login shell at a mise-installed binary, and the install path of the `github:` backend is not stable. `--with fish` installs the shell; changing the login shell stays manual.
- `dev setup` got `--ssh HOST` (apply over plain ssh to any machine) next to `--via ssm|ssh`, and `--create-user` was not built (`--user` selects an existing user).
- The overlay merge drops duplicate keys and a second `[tools]` header; a test parses the merged result with `mise` when it is installed (this caught a duplicate `java` key).
- The `Applier`/`ApplyScript` parity is tested in `runner/applyscript_test.go`; `Applier` itself is unchanged.
- Binary size: 69.5 MB before, 72.7 MB after (host build, `-s -w`), +3.1 MB; the picker stays go-fuzzyfinder.
- Not run against real AWS; unit tests use fakes for EC2, EIC, SSM and CloudWatch. `TestMCPShimListsToolsAndRelays` in `runner` fails on the base commit as well and is unrelated.

### Removal (done)

`cmd/ssm`, its Taskfile tasks and variables, the README section, the ssm spec and plans under `docs/superpowers`, `.superpowers/sdd/progress.md` and the doc references are gone; `go mod tidy` changes nothing. The legacy-compat reads (the old `~/.config/ssm/history.json` and `# ssm-*` ssh markers) were **removed** with it, because the goal is that nothing refers to the tool; only the `ssm-managed` security-group rule cleanup (`dev setup --revoke-legacy-rules`) stays, since those rules may still exist in AWS accounts.

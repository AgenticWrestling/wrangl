# ADR 0002: Remote runners (driver sessions, credentials, lifecycle)

- Status: **Accepted, implemented**
- Task: Z84sEj, under "Remote runner mechanism" (z0rBI0) in "Remote agents and special runners" (BjrJ3Y)
- Builds on: [ADR 0001](0001-agent-definitions-vs-templates.md) (agents vs templates), per-run worktrees on shared runners (`runner/worktree.go`), sessions per runner (`max_sessions`), project x template caps (`project_template_limit`, `launched_for_project`), the controller's secret store (`<data>/secrets`, write-only over the API)
- Touches: `runner_template` (mode, connection), `runner` (no enrolment for remote targets), `fleet` (launch/health/reap), `scheduler` (driver placement), `runner` (session env, tools), `costs`, the runner terminal page

## Context

Some work cannot run on a Linux runner: building and testing iOS/macOS apps needs a Mac (EC2 Mac
dedicated hosts, 24 h minimum allocation); Android work needs an emulator (KVM, a big image, often
one at a time). The owner's decisions (z0rBI0) fix the shape:

1. The **agent runtime does not run on the target.** It runs in a separate session on a host runner
   (the **driver**), in an ordinary local worktree of the repo.
2. The target is used to build, test and run things, over **SSH** (or **ADB** for Android). It is not
   the editing filesystem.
3. SSH/ADB credentials live in the **secrets store** and reach the target through **SSM
   port-forwarding**, so there are no inbound ports.
4. The target does **not** run the wrangl binary. A "remote runner" is therefore a
   **controller-owned record**: launch, health, idle reaping, cost and a terminal all come from the
   controller.
5. The template gets `mode: local|remote` and a connection block. `max_sessions` still limits
   sessions on a remote runner (an emulator may be 1).

What exists today and constrains the design:

- A runner is an EC2 box that runs `wrangl runner run`: it enrols, heartbeats over NATS, takes
  `cmd.assign`, hosts up to `max_sessions` sessions (each with its own worktree, PTY, hooks and
  logs keyed by run id), and self-upgrades when idle. The scheduler claims runners with
  `ClaimIdleRunner`/`FreeRunnerSlot` and launches boxes from queue demand. Idle reaping,
  drain and upgrade key off `runner.state`.
- The agent's tools reach wrangl through the local agent socket: `wrangl runner hook|mcp|tool`,
  routed by run id.
- Secrets reach a run in the assignment (env, or 0600 files removed when the run ends). The agent
  process can read its own environment and files; the GitHub token never goes to the agent.
- The controller already talks to AWS (EC2 launch/terminate, SSM parameters, Cost Explorer,
  instance inventory), and the Runners page and `costs` service attribute spend to instances.

The open design questions are: where the agent runs relative to the target (given as decided, but its
consequences need spelling out), how its commands reach the target, how credentials are stored and
rotated, how a tunnel is built, how the controller knows a target is alive, idle and costing money
without any code on it, and what happens when things break.

## Options

### Q1. How do the agent's commands reach the target?

**A. A plain `ssh`/`adb` on the driver, pre-wired by wrangl.** The session environment gets an ssh
config entry (`Host target` -> `127.0.0.1:<forwarded port>`, key file, host key pinned) and, for
Android, `ANDROID_ADB_SERVER_ADDRESS`/`adb connect` to a forwarded port. The agent just runs `ssh
target 'xcodebuild test ...'` or `adb shell`.
- Pros: nothing new for the agent to learn; works with every agent and its own shell tool; no
  wrangl code in the loop at command time; interactive-friendly (`ssh -t`).
- Cons: the agent holds a private key (or can read the key file). Output and exit codes are whatever
  ssh gives; no central audit of what ran on the target.

**B. An MCP tool (`target_exec`) served by `wrangl runner mcp`.** The agent calls
`target_exec {cmd, cwd?, timeout?}`; the runner daemon runs it over a connection it owns.
- Pros: the key never reaches the agent's process (it lives in the daemon, which is already
  trusted with the git token); every command is a structured event (audit, per-command timeout,
  output cap, cancellation, the live view); the same tool can work for SSH and ADB.
- Cons: agents without MCP (`completion: exit` agents) cannot use it; long builds need streaming
  and truncation rules; interactive programs (debuggers, `ssh -t`) do not fit; every agent has to be
  taught to prefer it over its own shell.

**C. Run the agent *on* the target (the rejected baseline).** Put the wrangl binary and the agent
on the Mac/emulator host as a normal runner.
- Pros: nothing new; the target's tools are local.
- Cons: contradicts decision 1 and 4; the repo and agent credentials would sit on an expensive
  host with a 24 h minimum; an emulator host cannot sensibly run the agent. Listed so the ADR
  records why not.

### Q2. How is the target reached without inbound ports?

**A. SSM port-forwarding session from the driver** (`aws ssm start-session --document-name
AWS-StartPortForwardingSession`), run by the runner daemon on the driver, one listener per
(session, target port) on loopback. The target needs the SSM agent and an instance profile (EC2
Mac and standard Android hosts can have both).
- Pros: no security-group ingress; auth is IAM, not network position; the same mechanism the
  project already uses for `cmd/ssm`.
- Cons: the driver's role needs `ssm:StartSession` on that instance; the session plugin must be on
  the driver; tunnels drop and have idle limits; one extra hop of latency.

**B. SSH over the VPC private network** (the driver and the target in one security group, port 22
allowed from it).
- Pros: simplest; no plugin; stable.
- Cons: opens an inbound port (to the VPC); makes network position part of the trust model; does not
  work for a target outside the VPC.

**C. A reverse tunnel started by the target** (the target dials out to the controller).
- Pros: works behind NAT.
- Cons: needs code or a service on the target (violates decision 4); a long-lived credential on
  the target.

### Q3. Where do credentials live, and who can read them?

**A. Controller secret store; the driver's daemon materialises them per session.** Template names
the secrets (`connection.ssh_key: secret-name`); at assignment the controller resolves them like
`secret_files` (0600, removed at run end) but into a **daemon-owned directory the agent user cannot
read** when the tools use Q1-B, or readable by the agent under Q1-A.
**B. Short-lived credentials minted at run time** (EC2 Instance Connect for SSH: push a one-time
public key through the API for 60 s; the private key exists only for that session).
- Pros: nothing long-lived to steal or rotate; revocation is automatic.
- Cons: needs an IAM path and the instance supports EIC (Linux hosts; **not** macOS), so it
  fits Android hosts and not Macs; a per-run key still has to be handled by the daemon.
**C. A key baked into the target image.** Rejected: not rotatable, readable by anyone with the AMI.

### Q4. How does the controller track lifecycle, idle and cost with nothing on the target?

**A. Controller-side probes, driver-side evidence.** The record has states
(`launching -> ready -> busy -> draining -> terminated`, plus `unreachable`). Health is an SSM
instance-information ping plus a probe through the tunnel when a session exists (ssh `true`/`adb
get-state`). "Busy" is derived from sessions: the controller already knows which runs reference the
remote runner. Idle time starts when the last session ends. Cost is the instance's own price
(already priced by the `costs` service) from launch (Mac: the 24 h dedicated-host minimum) to
terminate.
**B. A tiny agent on the target** (heartbeats, metrics). Rejected by decision 4.
**C. Use EC2 state only** (running/stopped, CloudWatch). Cheap but blind to "sessions in use"
and to a hung OS that EC2 still calls running.

## Decision

### 1. Where the agent runs

Keep decisions 1-5. A **driver session** is an ordinary session on an ordinary local runner
(any template with free slots). A run on a remote-mode template gets **two placements**: a **host
slot** on a local runner (the driver) and a **target** (the remote runner record). The run's
branch, worktree, PTY, hooks, logs and `max_sessions` accounting all live on the driver and are
keyed by run id as today, so the live-output card and terminal need no change ("where it runs" =
the driver). Which local template hosts the driver is a field of the remote template
(`connection.driver_template`, default: the project's default template), and the driver must
allow the run's agent (ADR 0001).

A remote runner's `max_sessions` caps how many driver sessions may *use* the target at once
(an emulator: 1). The driver slot and the target slot are claimed together in one transaction in
`assignOne`; if either is missing the run stays queued and the status says which ("waiting:
no free emulator").

### 2. How tools reach the target (Q1)

**Both A and B, with B the default where the agent supports MCP.**

- Every driver session of a remote run gets a wired **`target`** connection: a loopback port per
  target port, a pinned host key, and a generated ssh config (`Host target`) in the session's
  private home. A plain `ssh target ...` / `scp` / `adb` therefore works (A). This is the
  escape hatch and the interactive path.
- The runner MCP server adds `target_exec` and `target_put/get` (B). Credentials used by these live in
  the **daemon**, not the agent's environment: the daemon opens the SSH client itself. The
  ssh-config path in A uses a **per-session, per-run key** (see 3) that the agent can read; so
  what the agent could steal is the single-use key for this run, not the standing credential.
- The agent definition (ADR 0001) says which it prefers; the generated agent files tell it the
  target's hostname and how to reach it, and the system prompt for remote runs states that the repo
  is local, the target is for build/test/run only, and edits must be made locally.

### 3. Credentials (Q3)

- **Standing credential** (the SSH private key for a Mac, or the ADB key): in the controller secret
  store (`ssh_key_<template>`), write-only over the API, shown as set/missing on the Setup page
  next to the agent credentials. Referenced from the template's `connection` block by name.
- **Per-run credential**: for targets that support it (Linux/Android hosts via EC2 Instance
  Connect) the controller pushes a fresh public key for the run's duration; nothing standing is
  given to the driver. For macOS, which has no equivalent, the daemon holds the standing key
  and exposes only `target_exec` (Q1-B) by default; the plain-ssh path (Q1-A) then goes through a
  daemon-run **ssh-agent-style forwarder** (a local socket that signs, never reveals the key) rather
  than a key file. This keeps the key out of the agent's reach on every target type.
- **Rotation**: rotating is `wrangl secret set` plus a template bump; running sessions keep their
  open connection, new sessions use the new key. For EIC targets rotation is moot. The runbook
  adds a yearly reminder for standing keys, and Setup shows the secret's age (file mtime) as a hint.
- Host keys are pinned per remote runner record at launch (from the instance console output /
  first SSM-verified connection) and verified on every connect; a changed host key marks the
  target `unreachable` and refuses to connect, not trust-on-every-use.

### 4. Tunnels (Q2): SSM port-forward from the driver's daemon

- The daemon on the driver starts and supervises one `aws ssm start-session
  --document-name AWS-StartPortForwardingSession` process per (remote runner, port) it needs
  (22; 5555 for ADB), shared by every driver session on that host for the same target, listening on
  loopback only, with a random local port. Reference-counted: closed when the last session
  releases it.
- The driver's instance profile gets `ssm:StartSession` limited by tag to instances launched by
  this controller (`aws-iam.md` gets the statement). The SSM plugin is part of the driver
  templates' setup (documented, checked at launch of a remote run: missing plugin means the run
  stays queued with a clear status, not a mid-run failure).
- Fallback: a template may set `connection.via: vpc` to use direct SSH inside the VPC (Q2-B) for
  the cases where SSM is not available. It is documented as the less preferred path.

### 5. Lifecycle, idle and cost without the wrangl binary (Q4-A)

- The remote runner is a row in the same `runner` table with `mode = remote`, no NKey/enrolment,
  and the controller as its sole actor. Its state machine reuses the existing states so the
  Runners page, caps and reaper work unchanged: launching (EC2/Mac host allocation), `idle`
  when the health probe passes and no sessions use it, `busy` when any driver session references
  it, `draining`/`cordoned` as today, `offline` when probes fail, `terminated`.
- **Health**: a controller loop probes each remote runner every 30 s: SSM
  `DescribeInstanceInformation` (agent online) always, and a protocol probe through a tunnel while a
  session is using it. Three consecutive failures mark it `offline`; recovery marks it back
  (busy if sessions still reference it, as the sessions-while-offline fix already does for local runners).
- **Idle reaping**: the existing idle clock and `idle_timeout` apply to the record. Mac
  hosts: because of the 24 h minimum, reaping an idle Mac early saves nothing; the template's
  `idle_timeout` defaults to the remaining minimum (the controller records `allocated_at` and
  will not release a dedicated host before 24 h unless forced) and caps default to 0 (opt-in,
  already decided).
- **Cost**: unchanged service, one more source: the target instance's hourly price from launch
  (or host allocation) to termination, attributed to the project that launched it
  (`launched_for_project`) and shown on the Runners page like any runner. Dedicated-host
  allocation is a separate line item with its minimum.
- **Terminal**: the runner terminal page for a remote runner opens an `ssh` session through the
  tunnel, run by a driver daemon on behalf of the controller (watch-only by default, like run
  tabs), since the target has no wrangl daemon to host a PTY.
- **Drain/upgrade**: no binary to upgrade on the target; template version bumps apply by
  replacing the target (drain, launch a new one) unless the template says `reuse: true`.

### 6. Failure modes

| Failure | What happens |
|---|---|
| **Tunnel drops** | The daemon restarts the SSM session with backoff (up to ~1 min). In-flight `target_exec` calls fail with a distinct "connection lost" error the agent can retry; plain `ssh` sessions die as ssh normally does. The run is not failed by a drop alone. After the backoff window the target is probed; if it answers the run continues. |
| **Target gone** (instance terminated, host reclaimed, emulator crashed) | Probes fail, the record goes `offline`, then `terminated` when EC2 says so. The controller sends the driver sessions a `target.lost` event: the in-flight `target_exec` returns "target lost", the agent is told once (a hook message) and given one chance to finish or stash local work, then the run is **failed as `target_lost` (not the agent's fault)**. Local commits are checkpointed and pushed as for any failed run; the run can be re-queued, which acquires a new target. |
| **Driver dies** | As today for any runner: the run fails when the runner goes offline past the grace period; the target's session reference is released so it can idle. |
| **Controller restarts** | Remote runner records and their tunnel ownership are reconstructed from DB and driver heartbeats (drivers report open tunnels); orphan tunnels with no matching run are closed. |
| **Host key changes** | Connection refused, record `unreachable`, owner notified (Setup shows it); never auto-trusted. |
| **Credential missing/invalid** | Caught before claiming: queue-time status "waiting: secret ssh_key_mac missing" (same pattern as a missing agent secret). |
| **Cost runaway** | Caps (project x template), `max_runners`, and a hard `max_age` on the record (default 48 h) with a notification before termination. Dedicated hosts are never auto-released inside their minimum without the owner. |
| **Target leaks state between runs** | Per-run cleanup is the template's `connection.reset` script (run through the tunnel at release); `reuse: false` replaces the target instead. |

## Consequences

Good:
- The wrangl binary and agent credentials stay off expensive or exotic hosts; targets need only SSM and sshd/adbd.
- Driver sessions are normal sessions, so logs, terminal, worktrees, caps and the live-output card all keep working.
- No inbound ports; IAM governs reachability.
- The standing key can stay out of the agent's reach on every target type.

Costs and risks:
- Two-sided placement (driver slot plus target slot) is new scheduler logic and a new failure class
  ("waiting for a target"). It must be claimed atomically.
- A new daemon feature set: tunnel supervision, a forwarder, `target_exec` with streaming/limits.
  This is the bulk of the work.
- macOS has no short-lived-credential option; the daemon-held key plus signer-forwarder is more
  moving parts than a key file.
- The controller now needs probes and per-protocol logic for targets it cannot instrument. Health is
  weaker than a heartbeating runner; "busy" is inferred from sessions rather than observed.
- Dedicated Mac hosts make mistakes expensive (24 h minimum); the opt-in default cap of 0 and the
  `max_age` notification are the safety rails, not a solution.
- The SSM plugin and an IAM statement become requirements of driver templates.

## Out of scope / follow-ups

1. The template schema (`mode`, `connection`: driver_template, protocol, ports, secrets, via, reset, reuse, max_age) and migration.
2. Android specifics: emulator image management, snapshots for fast reset, `adb` auth key handling.
3. A shared pool of targets across projects and fair queuing (today: cap per project x template).
4. Targets outside AWS (an office Mac mini) via a different tunnel (Q2-C with a sealed agent); not designed here.
5. Recording `target_exec` output as a first-class artefact on the run page.

## Questions for the owner

1. Is `target_exec` (MCP) the primary path with plain `ssh` as the escape hatch (as written), or should plain `ssh` be primary and MCP optional?
2. Is the "run fails as `target_lost`, can be re-queued" behaviour right, or should a lost target pause the run and wait for a replacement?
3. For Macs, accept the daemon-held key + signing forwarder complexity, or accept a per-run key file readable by the agent (simpler, weaker)?
4. Should `max_age` (default 48 h) auto-terminate non-dedicated targets, or only notify?

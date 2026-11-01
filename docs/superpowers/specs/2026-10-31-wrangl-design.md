# wrangl — Controller / Runner Design

**Date:** 2026-10-31
**Status:** Draft rev 2. Owner decisions from two rounds are folded in (§16). Awaiting review.

## 1. Overview

`wrangl` is a single Go binary that runs in one of two **modes**, plus a set of **client**
commands:

- **Controller** — runs on a small dedicated EC2 host. Owns the SQLite database, an embedded
  NATS server with JetStream, the EC2 provisioner, the scheduler, the work stack (hierarchical
  tasks), GitHub integration through two Apps (author and reviewer), the HTTP API, and the
  mobile PWA served through Cloudflare Zero Trust.
- **Runner** — runs on EC2 instances that the controller launches. It enrols itself, applies
  its RunnerTemplate (a mise config plus a setup script), and works through one assigned task
  subtree at a time. It drives a coding agent in a PTY, streams output and hook events over
  NATS, and pushes one PR per assignment.
- **Client** — CLI and TUI commands that talk to the controller's HTTP/2 + SSE API. The PWA
  uses that same API surface.

It lives in the `devhost` repo next to `ssm`, as `cmd/wrangl`.

### Goals

- Set up tooling on remote machines declaratively, including coding agents, from a RunnerTemplate.
- Have the controller launch EC2 capacity that meets each template's minimum hardware spec,
  and assign work to it.
- Keep a **hierarchical, ranked work stack** with precursor dependencies. Humans edit it from
  the CLI, TUI and PWA, and agents edit it through MCP tools.
- Show everything running and queued across all runners. Cancel runs, watch read-only terminal
  output, and search/filter logs. Manage projects and repos, and import GitHub issues.
- PR flow: the runner pushes a PR, the reviewer App reviews it, low-risk changes auto-merge,
  and everything else becomes a review question with a web push notification.
- Support any agent, wired in through template-defined commands, hooks and MCP.

### Non-goals (v1)

- Multiple users or RBAC. There is one owner, and Cloudflare Access is the identity.
- Splitting one task subtree across runners, or running more than one assignment per runner.
- Interactive terminal input from the UI. The PTY view is read-only, and agents ask for human
  input through **Questions**.
- A remote MCP endpoint for the owner's own agents. That is v2 (§15, M10).
- Non-GitHub forges, non-Linux runners, multi-region fleets, and HA for the controller.

## 2. Topology

```
                         Cloudflare Zero Trust (Access; /gh/webhook bypass + HMAC)
 phone (PWA) / laptop / GitHub ──────────▶ cloudflared (on controller host, outbound only)
                                                 │ http://127.0.0.1:8080
┌──────────────────────── VPC / shared SG ───────┼─────────────────────────────────┐
│  ┌──────────────── controller host (small EC2) ▼───────────────┐                  │
│  │ wrangl controller serve                                      │                  │
│  │  ├─ HTTP API + SSE + PWA + /gh/webhook   127.0.0.1:8080      │                  │
│  │  │                                       + /run/wrangl/api.sock                 │
│  │  ├─ enrol listener               <private-ip>:4223           │◀── enrol, binary │
│  │  ├─ embedded NATS + JetStream    <private-ip>:4222 (TLS)     │◀── runners       │
│  │  ├─ SQLite (WAL) + sqlc          /var/lib/wrangl/wrangl.db   │                  │
│  │  ├─ scheduler · provisioner (EC2 API, instance role)         │                  │
│  │  └─ GitHub Apps · review orchestration · web push            │──▶ GitHub, push  │
│  └──────────────────────────────────────────────────────────────┘    services      │
│        ▲ NATS                         ▲ NATS                         (via NAT)     │
│  ┌─────┴───────────┐          ┌───────┴─────────┐                                  │
│  │ runner (EC2)    │   …      │ runner (EC2)    │  wrangl runner run (systemd)     │
│  │  PTY ▸ agent    │          │  PTY ▸ agent    │  agent ⇄ wrangl runner mcp       │
│  │  hooks ▸ wrangl runner hook│                 │  hooks ⇄ wrangl runner hook      │
│  └─────────────────┘          └─────────────────┘                                  │
└────────────────────────────────────────────────────────────────────────────────────┘
```

- Nothing listens on the internet. Browsers, the remote CLI and GitHub webhooks all arrive
  through `cloudflared`.
  - **Why not direct HTTP/3?** The phone only ever talks to Cloudflare's edge, and that link
    already uses HTTP/3 (QUIC), so going direct gains nothing for battery.
  - Going direct would put a public UDP/TCP 443 on the box that holds the EC2 launch rights,
    the GitHub App keys and the NATS operator keys.
  - Web push, not open sockets, is what makes background updates battery-efficient.
- The SG references itself, so runners can reach the controller on 4222 (NATS) and 4223
  (enrolment and binary download). Runners expose no ports.
- Runners need outbound internet access (mise, GitHub, the model provider). The controller
  needs outbound access for GitHub, web push and the EC2 API.

## 3. Command tree (cobra)

Tasks are addressed by **sqid** everywhere, for example `wrangl task show k3Xp9a`.

```
wrangl
├── controller                          # ── mode: on the controller host
│   ├── init      [--data-dir /var/lib/wrangl] [--config /etc/wrangl/controller.yaml]
│   │             # config skeleton, NATS operator/account keys, NATS TLS cert, VAPID keys,
│   │             # sqids alphabet, DB + migrations. Idempotent; never overwrites.
│   ├── serve     [--config] [--data-dir]
│   ├── migrate   [up|down|status]
│   ├── backup    <path>                # VACUUM INTO
│   └── secret    set|list|rm <name>    # value on stdin → <data-dir>/secrets/<name> (0600)
│                 # e.g. `claude setup-token | wrangl controller secret set claude_oauth_token`
│
├── runner                              # ── mode: on a runner instance
│   ├── bootstrap --controller <http://ip:4223> --token <wre_…> [--sha256 …]
│   ├── run       # daemon (systemd ExecStart)
│   ├── hook      <event> [--continue-format <agent>]   # hidden; agent hook entrypoint
│   ├── mcp       # hidden; stdio MCP server spawned by the agent
│   └── status
│
│   # ── client commands (HTTP API; the TUI/PWA use the same endpoints) ──────────────
├── tui
├── project   create <slug> [--name --template --auto-merge --auto-queue --priority]
│             list | show | edit | delete
├── repo      link <project> <owner/name> [--branch] | unlink | list [--project]
├── issue     sync <project> | list | show | to-task <issue> [--parent <sqid>]
├── task      create <project> --title [--prompt | -f prompt.md] [--parent <sqid>]
│                    [--after <sqid> | --before <sqid> | --first]   (default: last)
│                    [--repo --template --issue] [--needs <sqid>,…]
│             tree [<project> | <sqid>] [--all]       # ranked tree with state + blockers
│             list [--project --state --parent] | show <sqid>
│             edit <sqid> [--title --prompt -f]
│             move <sqid> [--parent <sqid> | --root] [--after | --before | --first | --last]
│             needs add|rm <sqid> <precursor-sqid>
│             split <sqid> [-f outline.md | --title … --title …] [--chain] [--copy-prompt]
│             split <sqid> --agent [--guidance "…"]   # queue a plan run (§8)
│             queue <sqid>                            # create a work run for this subtree
│             complete | reopen | cancel <sqid> [-m reason] | delete <sqid>
├── run       list [--state --runner --project] | show | cancel <run>
│             attach <run> | diff <run> [--stat] | files <run> [path] | cat <run> <path>
├── logs      <run> [-f] [--grep RE] [--stream agent|runner|hook|system] [--since]
│             search <fts-query> [--project --runner --since --limit]
├── pr        list | show <pr> | approve <pr> | request-changes <pr> -m … | close <pr>
├── question  list [--open] | show <id> | answer <id> (<option-value> | --text …)
├── template  create -f template.yaml | list | show | edit <name> | delete
├── fleet     list | show <runner> | launch --template <name> [--count]
│             cordon | uncordon | drain | terminate <runner>
│             enroll-token --template <name> [--ttl 30m]
├── push      test
└── config    use <url> | show

global flags:  --controller <url|unix:///run/wrangl/api.sock>   -o table|json|yaml   --config
```

`wrangl runner …` means *being* a runner. `wrangl fleet …` means *managing* runners.

**Client transport.** On the controller host the client uses `unix:///run/wrangl/api.sock`,
which is trusted through file permissions (`wrangl` group). Anywhere else it uses
`https://<cf-hostname>` with a Cloudflare Access service token, and the controller verifies
the resulting `Cf-Access-Jwt-Assertion` (§11).

## 4. Package layout

```
cmd/wrangl/
  main.go
  internal/
    cli/          cobra commands, one file per noun
    client/       typed HTTP API client + SSE reader (cli, tui)
    api/          HTTP handlers (JSON + SSE), CF Access verification, /gh/webhook
    web/          Datastar views (templ), static, manifest, sw.js
    tui/          bubbletea dashboard
    db/           migrations/ (goose, embedded) · queries/ · sqlc.yaml · dbgen/ (committed) · db.go
    stack/        work-stack domain: create/move/rank/needs/complete rules, sqids, fractional
                  ranks, cycle + scope checks, audit events. Shared by api and the MCP bridge
    controller/   server · natsd · enroll · scheduler · provision · ingest · gh · review · webpush
    runner/       bootstrap · daemon · template · workspace · pty · driver · hooks · mcp
    proto/        NATS subjects + message structs shared by both modes
```

The `stack` package is the **only** writer of task rows. The HTTP API (humans) and the NATS
bridge (agents) both call it, so the rules and the audit trail stay identical.

## 5. Data model (SQLite, sqlc)

- `modernc.org/sqlite` (pure Go), `goose` migrations run on `serve`, `sqlc` sqlite engine.
- Pragmas: WAL, `foreign_keys=ON`, `busy_timeout=5000`, `synchronous=NORMAL`. One
  single-connection writer pool and a reader pool.
- Timestamps are RFC 3339 UTC `TEXT`. Booleans are `INTEGER` 0/1. Structured data is JSON `TEXT`.
- Secrets live in `<data-dir>/secrets/` with mode 0600 and never go in the DB.

| Table | Purpose / key columns |
|---|---|
| `meta` | key/value: `sqids_alphabet`, schema notes |
| `runner_template` | `name`, `mise_toml`, `setup_script`, `agent_json` (§8), `min_vcpu`, `min_ram_mib`, `min_disk_gib`, `arch`, `instance_types`, `ami`, `max_runners`, `idle_timeout_s`, `version` |
| `enrollment_token` | `token_hash`, `template_id`, `instance_id`, `expires_at`, `used_at`, `runner_id` |
| `runner` | `name`, `template_id`, `template_version`, `instance_id`, `instance_type`, `az`, `private_ip`, `arch`, `vcpu`, `ram_mib`, `disk_gib`, `nkey_public`, `state`, `max_concurrent`, `last_heartbeat_at`, `last_metrics_json` |
| `project` | `slug`, `name`, `default_template_id`, `plan_template_id`, `auto_merge`, `protected_paths`, `max_auto_merge_lines`, `priority` (plus `auto_queue` in M4) |
| `repo` | `project_id`, `owner`, `name`, `default_branch`, `gh_installation_id` |
| `issue` | `repo_id`, `gh_number`, `title`, `body`, `state`, `url`, `synced_at` |
| `task` | `sqid` (unique, immutable), `project_id`, `parent_id`, `rank`, `repo_id?`/`template_id?` (NULL = inherit), `issue_id?`, `title`, `prompt`, `state`, `created_by`, `completion_summary`, `completed_run_id` |
| `task_dependency` | (`task_id`, `depends_on_id`): precursors, same project |
| `task_event` | audit: `task_sqid`, `run_id?`, `actor`, `op`, `diff_json`, `ts` |
| `run` | `task_id` (assignment root), `current_task_id`, `runner_id`, `kind` (`work`/`review`/`revise`/`plan`), `attempt`, `state`, `branch`, `session_name`, `base_sha`, `head_sha`, `status_text`, `exit_code`, `error`, timestamps |
| `pull_request` | `task_id` (root), `repo_id`, `run_id`, `gh_number`, `url`, `head_branch`, `state`, `risk`, `review_summary_md`, `gates_json`, `decision` |
| `log_line` + `log_line_fts` | `run_id`, `seq`, `ts`, `stream`, `level`, `message`; FTS5 external-content index |
| `hook_event` | `run_id`, `seq`, `ts`, `name`, `payload_json` |
| `question` / `question_option` | `kind` (`agent`/`pr_review`/`approval`/`plan_review`/`system`), prompt, options, `payload_json` (e.g. a proposed split tree), answer |
| `push_subscription` | Web Push endpoint + keys |

### Work stack rules

These are enforced in `stack` and backed by the DB wherever possible. **(DB)** marks a rule
enforced by a constraint or trigger, tested in the M1 scratch check.

- **sqid.** Encoded from the row id with `sqids-go`: minimum length 6, with an alphabet shuffled
  once at `init` and stored in `meta`. It's written in the creating transaction and stored, so
  it never changes even if the alphabet does. A trigger rejects any change. **(DB)**
- **Ranks.** Fractional-index strings over `0-9A-Za-z`, unique among siblings. Top-level tasks
  rank within their project. Partial unique indexes cover both the `parent_id IS NULL` case and
  the non-NULL case. **(DB)** Moving or reranking rewrites one row. "Rank order" means
  depth-first, siblings by rank; queries order by a path of ranks joined with a space, which
  sorts below every rank character.
- **Hierarchy.** A parent must be in the same project **(DB)** and can't be moved under its own
  descendant (recursive check). Deleting a task cascades to its subtree **(DB)**. Deleting needs
  approval when agent-initiated (§8).
- **Containers.** A task with children has no work of its own; its `prompt` is context passed
  to descendants. It derives its state: `done` once no child is open, in progress or in review
  and at least one child is done; `cancelled` if all children are cancelled; otherwise it
  follows its children. It can't be queued unless it has an open descendant.
- **Precursors.** Any task in the same project **(DB)**, never itself **(DB)**, and never an
  ancestor or descendant. A recursive reachability check rejects cycles.
  - A precursor counts as **satisfied** when it is `done` (merged), or when it was completed
    earlier **in the same run**, because that work is already on the same branch.
  - A precursor on a container means the whole subtree must finish.
- **Leaf state.** `open → in_progress → in_review` (the agent completed it; its PR isn't
  merged) `→ done` (merged). Other paths: `→ cancelled`, and `reopen` returns it to `open`.
- **Repo and template inheritance.** The effective value is the task's own, else the nearest
  ancestor's, else the project default. A queued subtree must resolve to **exactly one repo**;
  otherwise `task queue` refuses and names the single-repo child subtrees to queue instead.
- **Locking.** A subtree is locked while any non-terminal run is rooted at an ancestor-or-self
  or a descendant **(query tested)**. Locked subtrees can't be queued again. Agents and the
  owner can still edit inside a locked subtree, and edits inside it fall under the agent's scope.

### State machines

```
runner:  launching → enrolling → provisioning → idle ⇄ busy ;  cordoned · draining · offline → terminated
run:     queued → assigned → preparing → running ⇄ needs_input → pushing → succeeded
                                          ↘ blocked (no ready leaf; waiting on outside precursors)
         any non-terminal → cancelling → cancelled ;  → failed
```

The controller owns every transition. Runners report facts. The DB is the source of truth,
and NATS is only transport.

## 6. Scheduling and provisioning

**Queueing.** `task queue <sqid>` (or `project.auto_queue`, which queues top-level tasks in
rank order) creates a `work` run rooted at that task. It's refused if the subtree is locked,
resolves to more than one repo, or has no open leaf.

**Scheduler loop** (event-driven plus a 10 s tick):
1. Order queued runs by `project.priority DESC`, then root rank, then `queued_at`.
2. Skip a run if its subtree has no **ready** leaf for a fresh run, meaning every open leaf
   waits on a precursor outside the subtree that isn't `done`. Show it as `waiting on <sqids>`.
3. Pick an `idle` runner on the matching template at the current version, and send
   `cmd.assign`. An ack moves the run to `assigned`; a timeout puts it back in the queue.
4. If no runner matches, and that template has fewer than `max_runners` runners including ones
   still launching, launch one.
5. Reap drained runners once they go idle. Idle shutdown is a separate loop (below).
6. When a precursor becomes `done`, re-evaluate `blocked` runs (§8) and waiting queued runs.

**Launch** (built in M2):
- **Instance type:** the template's `instance_types` allowlist if set (first type meeting the
  minimums, in preference order), otherwise `DescribeInstanceTypes` filtered by arch,
  current-generation and the minimums, smallest by vCPU and then memory.
- **Instance settings:** a root gp3 volume of `min_disk_gib` (encrypted, `DeleteOnTermination`),
  IMDSv2 required, and `InstanceInitiatedShutdownBehavior=terminate`.
- **User-data** fetches `wrangl-linux-$(uname -m)` from `:4223`, checks its sha256 (pinned per
  arch in the script), and runs `wrangl runner bootstrap`. Inputs are validated before
  interpolation.
- **AWS credentials:** the controller uses the SDK's default credential chain, which is the
  controller host's **instance role**. No keys go in config. A least-privilege policy is in
  `docs/wrangl/aws-iam.md`.

**Tags.** Every instance, root volume and network interface is tagged `Name=wrangl-<runner>`,
`wrangl:managed=true`, `wrangl:controller=<id>`, `wrangl:runner` and `wrangl:template`. The
controller id is random, made at `init` and stored in `meta`. Before every terminate call, the
controller re-reads the live instance and refuses (`ErrNotOwned`) unless it carries
`wrangl:managed=true` **and** this controller's id.

**Idle shutdown.** The reaper runs every 30 s and terminates *controller-launched* runners in
these cases:
- `idle` for longer than the template's `idle_timeout` (**default 30 min**)
- `offline` for longer than that timeout
- still `launching`/`enrolling` 15 min after launch

`idle_since` survives an offline blip, so a flaky network can't keep an idle runner alive. In
M4 it is cleared while a runner is busy. Adopted runners are never reaped. Cordon is a flag,
not a state, so a cordoned idle runner is reaped like any other. Drained runners are
terminated as soon as they're idle.

**Teardown and orphan safety nets.**
- `wrangl fleet teardown` calls AWS **directly**, with local credentials and `--region` or
  `--profile`, so it works when the controller is gone. It lists the tagged instances, asks for
  confirmation and terminates them, scoped to one controller id (read locally on the controller
  host, or `--controller-id`) or `--all-controllers`. Untagged instances are never touched.
- A runner that can't reach its controller for **2 h** powers itself off, which terminates the
  instance.

### Runner self-upgrade

A runner reports the sha256 of its own binary in each heartbeat (`binary_sha`). The hash is
the version, because dev builds all say `dev`. When it differs from the binary the
controller serves for the runner's arch:
- The controller sets `runner.upgrade_sha`. `ClaimIdleRunner` skips runners with an
  upgrade pending, so no new run lands on the old binary.
- Once the runner is idle, the controller sends `cmd.upgrade {sha256}`. It resends after
  2 minutes without progress.
- The runner downloads `<enrol URL>/dl/wrangl-linux-<arch>` itself. The URL comes from its
  own config, never from the message. It verifies the hash, renames the file over its
  executable, and `exec`s it.
- Assignment and upgrade decide under one lock, so a run can't start mid-upgrade.
- A failed restart keeps reporting the old hash, so the controller keeps retrying instead
  of believing it.
- Self-upgrade is on only for `wrangl runner run` (`DaemonOptions.Upgrade`). Runners that
  don't report a hash are left alone.

## 7. Enrolment and NATS security

Built and tested in M2.

- **Operator/JWT mode.** Keys are generated at `init` under `nats/` (0600): the operator, a
  `SYS` account, and the `WRANGL` account with a separate signing key. JetStream limits are set
  on the account claims. Runner user JWTs are signed by the signing key, so adding a runner needs
  no server reload. The controller's own user is minted fresh at every start and never written
  to disk.
- **TLS.** A CA is created at `init`. `serve` issues a new leaf at each start for the configured
  host plus loopback. TLS also applies to the controller's *in-process* connection: without a
  client TLS config, nats.go deadlocks closing the pipe (found in M2).
- **Enrolment steps:**
  1. The runner generates its NKey; the private seed never leaves the box.
  2. It reads its instance id from IMDSv2, which is empty off EC2.
  3. It sends `POST :4223/enroll {token, nkey_public, instance_id, facts}`.
  4. The controller checks the token: hash, unused, unexpired.
  5. For controller-launched runners it also checks that the token is bound to that
     instance id, **and** that the request's TCP source IP equals the instance's private IP.
     Inside the VPC there's no NAT, so this proves the request came from that instance. This
     replaces the rev 1 plan of verifying the identity-document signature, which would have
     needed embedded per-region AWS certificates.
  6. It checks arch and size against the template minimums. RAM and disk get 10 % tolerance,
     since the box reports less than the nominal size.
  7. It burns the token in the same transaction, and returns
     `{runner_id, name, user_jwt, nats_urls, nats_ca}`.
  8. Bootstrap writes `/etc/wrangl/runner.{yaml,creds}` and `ca.pem` (root-owned, creds 0600),
     creates the unprivileged `agent` user, then installs and starts `wrangl-runner.service`
     (runs as root; see "Runner users" in §8).

| Subject | Dir | Kind |
|---|---|---|
| `wr.r.<r>.cmd.{assign,cancel,template,answer,diff,tree,file,stack.reply}` | C→R | request/reply |
| `wr.r.<r>.hb` | R→C | core, every 10 s + metrics |
| `wr.stack.<r>.<op>` | R→C | request/reply: work-stack tool calls (§8), answered by `stack` |
| `wr.evt.<r>.<x>.{state,log,hook,question,task}` | R→C | JetStream `WR_EVT` (7 d) |
| `wr.pty.<r>.<x>` | R→C | JetStream `WR_PTY` (2 GiB / 3 d) |

Runner `<r>` may subscribe only to `wr.r.<r>.cmd.>` and `_INBOX_<r>.>`. It may publish only to
`wr.evt.<r>.>`, `wr.pty.<r>.>`, `wr.stack.<r>.>`, `wr.r.<r>.hb` and the JetStream publish-ack
API. The controller derives the runner, and so the run and its scope, **from the subject**,
never from the payload, so a runner can't act on another runner's subtree.

## 8. Runner: templates, agents, the work loop

### Template

```yaml
name: go-claude
arch: arm64
min: { vcpu: 4, ram: 16GiB, disk: 100GiB }
instance_types: [m7g.xlarge, m7g.2xlarge]
max_runners: 3
idle_timeout: 30m
mise_toml: |
  [tools]
  go = "1.26"
  node = "24"
  "npm:@anthropic-ai/claude-code" = "latest"
setup_script: |
  sudo dnf install -y git tmux
agent:
  drive: headless                       # headless (default) | interactive
  secrets: { CLAUDE_CODE_OAUTH_TOKEN: claude_oauth_token }   # env ← controller secret
  start:  ["claude", "-p", "--session-id", "{{.SessionUUID}}", "--permission-mode", "bypassPermissions", "{{.Prompt}}"]
  resume: ["claude", "-p", "--resume", "{{.SessionUUID}}", "--permission-mode", "bypassPermissions", "{{.Prompt}}"]
  turn_timeout: 2h
  remote_control: { enabled: false }    # experimental, see below
  files:
    - path: "{{.Workdir}}/.claude/settings.local.json"
      content: |
        {"hooks": {
          "PostToolUse":  [{"hooks":[{"type":"command","command":"wrangl runner hook PostToolUse"}]}],
          "Notification": [{"hooks":[{"type":"command","command":"wrangl runner hook Notification"}]}],
          "Stop":         [{"hooks":[{"type":"command","command":"wrangl runner hook Stop"}]}]}}
    - path: "{{.Workdir}}/.mcp.json"
      content: '{"mcpServers":{"wrangl":{"command":"wrangl","args":["runner","mcp"]}}}'
```

- Template variables: `Prompt`, `Workdir`, `RunID`, `TaskSqid`, `SessionName`, `SessionUUID`,
  `Branch`, `Repo`.
- **Applying a template** (built in M3). Every step is skipped when already done, so
  re-applying is safe:
  1. install mise to `/usr/local/bin` if missing (download the official installer to a file
     and run it, never `curl | sh`)
  2. run `setup_script` as root if its sha256 changed
  3. write `~agent/.config/mise/config.toml`
  4. run `mise install --yes` as `agent`

  Reconciliation is driven by heartbeats, which carry the applied version and whether an apply
  is running. When a runner is behind, the controller claims it (`provisioning`) and sends
  `cmd.template`. The result goes back durably over JetStream (`wr.evt.<r>.template`).
- **A failed apply** records the error and the last 8 KiB of output on the runner, cordons it,
  and marks the template version failed. The scheduler won't launch for a failed version, the
  idle reaper still removes the box after the timeout, and `template edit` clears the failure.
- Agents stay generic. Codex fits the same shape (`codex exec …` / `codex exec resume …`).
  Antigravity gets its own template once its headless interface is checked.

### Runner users

There are two identities on every runner (D26):

- **The daemon runs as root.** It holds the NATS credentials
  (`/etc/wrangl/runner.creds`, root 0600). It applies templates: it installs mise to
  `/usr/local/bin` and runs the template's `setup_script` as root, since templates are
  owner-authored. It powers the box off when orphaned.
- **The agent runs as `agent`,** an unprivileged user with no sudo and its home in
  `/home/agent`. mise tools are installed into *its* home (`~agent/.config/mise/config.toml`,
  `mise install` run as `agent`). Worktrees live under `~agent/work`, and the daemon starts the
  agent, its hooks and the MCP shim as this user.
- **What that buys:** a subverted agent can't read the runner's NATS credentials, so it can't
  fake runner events or work-stack calls by going around the MCP layer. Its only channel to the
  daemon is the local socket `/run/wrangl/runner.sock`. From M5 the daemon authenticates callers
  on that socket by peer UID (`SO_PEERCRED`) and accepts only the `agent` UID for the run in
  progress.
- **What the agent can still reach:** the model token and the per-run GitHub token in its own
  environment, and anything the runner's network allows. That's the blast radius the
  disposable runner exists to contain.

### Agent credentials

The owner runs `claude setup-token` on the controller host and pipes the token into
`wrangl controller secret set claude_oauth_token`. That token is valid for a year, makes model
requests only, and needs a Pro/Max/Team/Enterprise plan.

Each `cmd.assign` carries the secrets the template asks for, over NATS TLS. The runner puts
them only in the agent's process environment, never on disk. Every runner draws on the same
subscription's limits. Other agents use the same mechanism with their own secret names.

### Session naming and Remote Control

- Every run gets `session_name = "<runner>:<root-sqid>"` and a fresh `SessionUUID`. Both are
  shown in the TUI and PWA.
- **Remote Control is optional and off by default.** Per the Claude Code docs, `setup-token`
  tokens "can only make model requests, so [they] can't establish Remote Control sessions."
  Remote Control needs a full claude.ai `/login`. Its server mode also prompts once to enable it
  and to trust the workspace, and has no documented unattended mode.
- **As built (M9):** there is no special `remote_control` switch (D30). Remote Control is an
  interactive-drive template:
  - `drive: interactive` starts **one** agent session for the whole assignment, in the PTY.
  - `secret_files` materialises the full login (for example
    `{{.Home}}/.claude/.credentials.json` from the controller secret `claude_credentials`). It
    is written 0600 for the agent, never logged, and deleted when the run ends. This is the one
    place a secret touches a runner's disk, and only because the agent can read a login
    nowhere else.
  - The start command carries whatever flag the agent uses to enable Remote Control.
  - `expect` rules (regexp → keystrokes, once each unless `repeat`) answer first-run prompts
    in the PTY.
  - Example: `docs/wrangl/templates/claude-interactive.yaml`. Its prompt texts, the Remote
    Control flag, and shared-login refresh behaviour are **unverified** (marked as such in the
    file). Use `max_runners: 1` until checked.

### The work loop

`cmd.assign` brings:
- the run and its root sqid
- the repo and base branch
- the template secrets
- a GitHub **author App** installation token scoped to the one repo
  (`contents:write`, `pull_requests:write`), delivered to git through a credential helper and
  never written to disk

Steps:
1. **Prepare.** A cached bare clone, then worktree `~wrangl/work/<run>`, then branch
   `wrangl/<root-sqid>-<attempt>`. Write the agent files.
2. **Loop** (headless drive, one agent session for the whole assignment):
   1. Ask the controller for the next ready leaf (`wr.stack.<r>.next`, which runs
      `NextReadyLeaf` with this run's id). Set `run.current_task_id` and move the leaf to
      `in_progress`.
   2. Build the turn prompt from the leaf's title and prompt, its ancestors' prompts as context,
      the summaries of siblings already completed in this run, and the standing instructions:
      commit your work, call `task_complete(<sqid>, summary)`, and use the stack tools to add
      or reorder subtasks if you find more work.
   3. Run `start` for the first turn and `resume` after that, in the PTY. PTY bytes go to
      `wr.pty`; lines with ANSI stripped go to `wr.evt.….log`.
   4. After the turn, check the leaf. If it's complete, the runner makes a checkpoint commit of
      anything left uncommitted (`[<sqid>] <title>`). If it isn't, nudge once with a follow-up
      turn. If it's still incomplete, raise a `system` Question: *Retry* · *Skip (leave open)* ·
      *Fail run*.
   5. Repeat. The agent may have added, reordered or completed subtasks meanwhile, so the next
      ready leaf is recomputed each time.
3. **End of loop.**
   - If the root has no open leaves: push, open the PR (as the author App) and move to
     `succeeded`. Completed leaves stay `in_review` until the PR merges.
   - If open leaves remain but none is ready (they wait on outside precursors) and there are
     commits: push and open the PR for the work so far, release the runner, and set the run to
     `blocked`. When the precursors are `done`, the scheduler queues a continuation run (a
     `revise`-kind run on the same branch and PR) automatically.
   - If there are no commits at all, the run moves to `blocked` without a PR.
4. **Interactive drive** (`drive: interactive`, work runs only; plan/review/revise stay
   headless) does the same loop inside one session. The `Stop` hook is the turn boundary:
   `wrangl runner hook Stop --continue-format claude` asks the daemon, which:
   - checkpoint-commits the current task;
   - if it isn't completed, nudges once in the session;
   - otherwise takes the next ready leaf and prints `{"decision":"block","reason":"<next
     prompt>"}`, which Claude Code treats as "keep going with this".

   With nothing left to hand out, the hook prints nothing (exit 0). The daemon then gives the
   session `InteractiveEndGrace` (5 s) to exit by itself and terminates it, because a TUI
   never exits. The run loop then carries on as in headless drive: a task still incomplete gets
   a headless-style nudge turn and then the owner question, and the next ready leaf (if the
   session stopped early) starts a resumed session. `--continue-format text` prints the bare
   prompt for agents whose stop hooks read stdout. The Claude format is unit-tested; the
   end-to-end path is tested with a fake session agent (trust prompt, login file, chaining,
   nudge, termination).
5. **Hooks** (`wrangl runner hook <event>`): read stdin JSON and post it to the daemon socket,
   which publishes `wr.evt.….hook`. They're fire-and-forget and never block the agent, except
   for the `--continue-format` mode above.
6. **Cancel:** SIGTERM to the PTY process group, then SIGKILL after 15 s. Leaves still
   `in_progress` revert to `open`.
7. **Inspection:** `cmd.diff`, `cmd.tree` and `cmd.file`, capped at 4 MiB. Worktrees are
   removed 24 h after the run ends, or sooner when disk passes 85 %.

### Plan runs ("Split with agent")

A `plan` run decomposes one `open` task into a proposed subtree. It makes no repo changes.

- **Queued by** the PWA's *Split with agent* button, `wrangl task split <sqid> --agent
  [--guidance …]`, or the TUI. The task is locked while the run is active, so nobody queues it
  or splits it by hand mid-plan.
- **Template:** `project.plan_template_id`, falling back to the work template. A cheaper or
  smaller template can be set for planning.
- **On the runner:** the repo is checked out read-only for context, and nothing is pushed. The
  agent gets the task, its ancestors' prompts, its sibling titles and the optional guidance.
  - Tool scope: the **read** stack tools, `ask_question`, `report_status` and `propose_split`.
    Every write tool is refused.
  - One turn, plus at most one nudge if the agent ends without proposing.
- **Result:** `propose_split` stores the tree in a `plan_review` Question's `payload_json`
  (options *Accept* · *Edit* · *Reject*) and sends a web push. The run then succeeds.
  - *Accept* applies the proposal through the same atomic `split` the manual path uses
    (`actor = agent:<run>`, approved by `user`).
  - *Edit* opens the Split sheet in the PWA pre-filled with the proposal, so you can tweak it
    and confirm.
  - *Reject* (with optional feedback) can re-queue another plan run with that feedback added to
    the guidance.
- **Staleness:** if the task changed after the proposal (it was edited, split by hand, or is no
  longer `open`), *Accept* and *Edit* still open the sheet but show a warning banner.

### MCP tools (`wrangl runner mcp`)

The MCP server is stdio, proxied through the daemon socket to `wr.stack.<r>.<op>`.

| Tool | Notes |
|---|---|
| `task_get(sqid)` | task, effective repo/template, precursors (with satisfied flag), children |
| `task_tree(sqid?, depth?)` | ranked subtree; default = assignment root |
| `task_list(filter)` | project-wide read: state, parent, text |
| `task_next()` | next ready leaf for this run (read-only peek) |
| `task_create(parent, title, prompt, position{first\|last\|after\|before}, needs[])` | |
| `task_update(sqid, title?, prompt?)` | |
| `task_move(sqid, parent?, position)` | no cycles; stays in project |
| `task_needs_add(sqid, precursor)` / `task_needs_remove(…)` | cycle-checked |
| `task_complete(sqid, summary)` | leaf only → `in_review`, `completed_run_id` = this run |
| `task_reopen(sqid)` / `task_cancel(sqid, reason)` | |
| `task_delete(sqid)` | see scope |
| `ask_question(prompt, options[], allow_free_text, detail_md)` | blocks; run → `needs_input` |
| `report_status(summary, percent?)` | |
| `submit_review(risk, summary_md, concerns[])` | review runs only |
| `propose_split(children[{title, prompt, children}], chain, rationale_md)` | plan runs only; ends the run |

**Agent scope.** Agents can **read** the whole project. They can **write** only inside their
assignment subtree:
- create, update, move or rerank, complete, reopen, add or remove precursors
- the precursor *target* may be anywhere in the project
- moving a task out of the subtree is refused

`task_cancel` and `task_delete` on a task the agent didn't create itself (`created_by` ≠
`agent:<this run>`) raise an `approval` Question for the owner. The call returns
`pending_approval`, and the outcome arrives as a later event. Every change writes `task_event`
with `actor = agent:<run id>`. The human API writes `actor = user`.

## 9. PR review and merge

There are two GitHub Apps:
- **wrangl-author** pushes branches and opens PRs. Permissions: contents and pull requests
  write, issues read.
- **wrangl-reviewer** reviews, approves and merges, and receives webhooks. Permissions: pull
  requests write, contents write (for merge), checks and issues read.

Using two Apps avoids GitHub's rule that a PR's author can't approve it.

1. **Gates**, computed by the controller:
   - CI green
   - diff size ≤ `max_auto_merge_lines`
   - no paths matching `protected_paths`
   - lockfile and binary limits
2. **Review run** on a runner (reviewer template, which defaults to the work template). The
   agent gets the PR diff and the completed-task summaries, and must call `submit_review`.
3. **Decision:**
   - **Auto-merge** when `auto_merge` is on, every gate passes and risk is `low`. The reviewer
     App approves and squash-merges, and every task this run completed moves to `done`
     (`MarkRunTasksDone`).
   - **Otherwise** a `pr_review` Question: *Approve & merge* · *Request changes* (free text) ·
     *Close*. A web push goes out. *Request changes* queues a `revise` run on the same branch
     with the notes and reopens the affected leaves; the cycle repeats.
4. **Webhooks.** GitHub posts to `https://<cf-hostname>/gh/webhook`, which a Cloudflare Access
   **bypass** policy covers. The controller verifies `X-Hub-Signature-256` with the reviewer
   App's webhook secret. Events: `check_suite`/`check_run` completed, `pull_request`
   (closed/merged/synchronize), `pull_request_review`, `issues`.
   - Deliveries are deduplicated by `X-GitHub-Delivery`.
   - A reconcile poll on start and every 15 min covers missed deliveries.
5. **Verify in M6:** that an approving review from a GitHub App satisfies a "required approvals"
   branch-protection rule. If it doesn't, give the reviewer App a ruleset bypass.

## 10. API, SSE, TUI

- **API:** JSON under `/api/v1/…`, mirroring the CLI nouns. Work-stack writes go through
  `stack` with `actor=user`. Besides plain CRUD, the stack exposes `move` (neighbours by sqid)
  and an atomic `split`, used by the PWA, the TUI and `wrangl task split` alike.
- **SSE:** `/api/v1/events` (global), `/api/v1/runs/{id}/pty` (replay via `Last-Event-ID`,
  then follow) and `/api/v1/runs/{id}/logs?grep=`. Every stream sends a keepalive comment every
  15 s, which keeps it alive through Cloudflare.
- **TUI** (`bubbletea` + `lipgloss`):
  - Panes: **Runners** · **Runs** (running, blocked, needs_input, queued) · **Work stack**
    (collapsible ranked tree with states, precursor markers and lock badges) · **Questions** ·
    **PRs**.
  - Keys: `a` attach · `l` logs (`/` filter) · `d` diff · `c` cancel · `n` new task (child or
    sibling) · `J/K` rerank · `>`/`<` indent/outdent · `N` add precursor · `Q` queue subtree ·
    `x` complete · `s` split · `p` projects · `r` link repo · `q` answer.
- **Remote MCP (M10).** `POST /mcp` serves the work-stack tools to the owner's own agents
  (MCP Streamable HTTP, JSON responses only; `GET` returns 405).
  - It is exposed like `/api/v1`: on the unix socket, and over TCP only behind Cloudflare
    Access. An agent off the box uses an Access service token.
  - Scope is `runs.UserScope()`: the owner as actor, every project, no subtree lock.
    Cancels and deletes apply directly, with no approval.
  - Tools: the stack CRUD set plus owner-only `project_list`, `task_queue` and `run_list`.
    `task_create` takes `project` for top-level tasks.
  - It does not offer `ask_question`, `report_status` or `task_next`, which only make sense
    inside a run, nor the review/plan tools.
  - `wrangl mcp` is a stdio bridge to `/mcp`. It uses the CLI's controller URL and service
    token, so `claude mcp add wrangl -- wrangl mcp` works anywhere the CLI does.

## 11. Web PWA (Datastar)

Server-rendered `html/template` pages (D29) made live by Datastar. Mobile-first (designed at
360–430 px, works on desktop) and installable.

**As built (M8):** every page opens one Datastar stream, `GET /ui/live?view=…`, that
re-renders the page's `<main id="main">` and morphs it in whenever the event bus fires
(debounced 250 ms, 15 s keepalive), plus the Inbox badge. Sheets (`<dialog>`) and the
terminal sit outside `#main`, so a live morph never closes a sheet or resets the terminal.
Actions are `@post('/ui/act/<action>')`. Datastar sends its signals as JSON, and the answer is
a toast fragment, optionally with `merge-signals` (clear the form) or `execute-script`
(navigate). Markdown is goldmark (GFM) sanitised by bluemonday. Files are highlighted by
chroma with classes, with light and dark styles at `/static/chroma.css`. Icons are an SVG plus
PNGs generated at `/icons/<n>.png`. The service worker handles push only and caches nothing.

All JS is vendored into the binary, pinned, with checksums in `internal/web/static/vendor/VENDOR.md`.
No CDN is used, so the PWA works behind Access with no third-party origins:
- **Datastar 1.0.0-beta.11** (the version the attribute syntax here targets).
- **SortableJS 1.15.7** for touch drag-and-drop. It uses `forceFallback` because native HTML5
  DnD is unreliable on iOS. A drop calls `POST /api/v1/tasks/{sqid}/move` with the new parent
  and the previous sibling.
- **xterm.js 6.0.0 + addon-fit 0.11.0** for the terminal, fed by the PTY SSE stream.

### Navigation

A bottom tab bar:

**Backlog** · **Runs** · **Runners** · **Inbox** (questions + PR reviews, with a badge count) · **More**

More holds projects, repos, templates and settings, including push subscription. Every push
notification deep-links into Inbox or into the item itself.

### Backlog (work-stack editor)

- **Scope:** a project picker at the top (remembered per device), plus "All projects", which
  orders by project priority with no cross-project dragging.
- **Rows** in rank order, indented by depth, with a collapse chevron on containers. Each row
  shows:
  - a drag handle and the sqid
  - the title
  - a state chip (`open`, `in progress`, `in review`, `done`)
  - badges: 🔗 *n* unmet precursors · 🔒 `<runner>` when its subtree is locked by a run ·
    children done/total on containers
- **Filters:** open-only by default; a toggle shows done and cancelled tasks.
- **Drag and drop:**
  - SortableJS nested lists. A long press (about 200 ms, touch only) starts a drag, so normal
    scrolling still works. Only the handle drags.
  - Dropping *between* rows reorders, possibly under a new parent. Dropping *onto* the middle
    of a row makes the task its last child.
  - On drop the client sends `POST /api/v1/tasks/{sqid}/move {parent, after|before: <neighbour
    sqid>}`. Neighbours are identified by **sqid, never by index**, so the move still makes
    sense if an agent reordered things meanwhile.
  - The server computes the new rank and the SSE stream patches the tree for every viewer. If a
    named neighbour has moved away, the task goes last under the target parent and a toast says so.
  - Rejected moves snap back with the reason: a cycle, moving out of a locked subtree, or
    moving an `in_progress`/`in_review` leaf. Allowed edits inside a locked subtree are picked up
    by the agent on its next turn.
  - **Accessible fallback:** a row's ⋯ menu has *Move up/down*, *Indent/Outdent* and *Move to…*
    (a searchable parent picker).
- **New task:**
  - A floating **+** button opens a bottom sheet with: title; prompt (markdown editor with an
    Edit/Preview toggle); parent (defaults to the current context, can be top level); position
    (top or bottom, default bottom); repo and template (inherited value shown greyed, can
    override); precursors (search by sqid or title).
  - Two buttons: **Create** and **Create & queue**.
  - **Quick add:** an inline "+ Add task" row at the end of each expanded container takes just
    a title (Enter adds another).
- **Split into subtasks** (⋯ → *Split*, on an `open` task):
  - The sheet has a multi-line editor with one subtask per line; indentation with two spaces or
    a markdown list nests them. A live preview tree sits beside it.
  - Options: *Chain sequentially* (each subtask needs the previous one) and *Copy parent prompt
    into each* (otherwise the prompt stays on the parent as shared context, since the parent
    becomes a container).
  - The split is **atomic**: `POST /api/v1/tasks/{sqid}/split {children:[{title, prompt,
    children}], chain}` runs as one transaction and writes one `task_event` per create.
  - Refused for `in_progress`/`in_review` tasks. Splitting a container appends the new
    children after its existing ones.
  - **Split with agent:** a button on the same sheet, with an optional guidance field. It
    queues a `plan` run (§8). The task shows a "planning on `<runner>`" badge. The proposal
    arrives in Inbox as *Accept* · *Edit* (opens this sheet pre-filled) · *Reject*.
- **Task detail** (tap a row): the prompt rendered as markdown; children; precursors and the
  tasks that depend on it (both editable); effective repo and template; linked issue;
  runs and the PR; the `task_event` history (who changed what: you or `agent:<run>`).
  Actions: queue, complete, reopen, cancel, delete, split.

### Runners

- **Header:** a demand strip, e.g. `go-claude: 2 busy · 1 idle · 1 launching · 3 queued runs
  waiting`, plus **Launch runner**. Launch opens a template picker that shows the minimum spec
  and the instance type it would choose.
- **Runner cards** show:
  - name and state chip
  - template and version, with a "template update pending" marker
  - instance type, AZ, uptime, or idle time against the idle-timeout countdown
  - **CPU, memory and disk** gauges with a 1-hour sparkline. History comes from an in-memory
    ring buffer of heartbeats on the controller (360 × 10 s per runner, not persisted).
  - the current assignment: root sqid and title, the subtask in progress, `status_text`,
    elapsed time, and `session_name`
- **Runner detail:**
  - The current run's **live terminal** (read-only xterm, tap for full screen, rotates to landscape).
  - The **assignment progress** list: the subtree with ✓ (`in_review`/`done`), ▶ (current),
    ⏸ (blocked, with its precursor) and ○ (pending). It updates as the agent edits the stack.
  - Recent runs on this runner, and a link to the template.
  - Actions:
    - *Cancel run*
    - *Cordon* / *Uncordon*
    - *Drain*
    - *Terminate*, whose confirmation names the run and any uncommitted work that would be lost
  - Offline runners show the last heartbeat time and the last metrics.

### Other views

- **Runs:** a list grouped by running, needs input, blocked, queued (with "waiting on
  <sqids>"), and recent. Run detail has the terminal, logs (search and stream filter), the hook
  timeline, the **diff** (chroma, unified or split, per-file collapsible) and the **file
  browser** with **markdown preview** (goldmark + GFM + bluemonday).
- **Inbox:** open questions (agent, approval, system) as cards with option buttons and an
  optional free-text box, and PR reviews (summary, gates, risk, link to the diff, actions).
  Answered items drop out of the list.

- **Web push (v1):**
  - VAPID keys from `init`, sent with `webpush-go`.
  - Notifies on: a question (agent, approval, PR review), a run failed or blocked, a runner offline.
  - Deep links into the app. Android shows actions; iOS needs the PWA installed (16.4+).
- **Cloudflare Access:**
  - Verify `Cf-Access-Jwt-Assertion` (team certs, `aud`, owner email) on every request except
    `/gh/webhook`, which uses HMAC.
  - `<link rel="manifest" crossorigin="use-credentials">`, or Access blocks the manifest fetch.
  - Use a long session duration.

## 12. Configuration

`/etc/wrangl/controller.yaml`:

```yaml
data_dir: /var/lib/wrangl
listen: { http: 127.0.0.1:8080, unix: /run/wrangl/api.sock, nats: 10.0.1.5:4222, enroll: 10.0.1.5:4223 }
aws: { region: us-west-2, subnet_ids: [subnet-…], security_group_id: sg-…, runner_instance_profile: wrangl-runner }
github:
  author_app:   { id: 123, private_key: secrets/gh-author.pem }
  reviewer_app: { id: 456, private_key: secrets/gh-reviewer.pem, webhook_secret: secrets/gh-webhook }
cloudflare: { team: myteam, aud: "…", owner_email: you@example.com }
web: { public_url: https://wrangl.example.com }
```

Paths are relative to `data_dir`. Agent secrets are managed with `wrangl controller secret`.
The client config `~/.config/wrangl/config.yaml` holds `controller` and an optional
`cf_service_token`.

## 13. Operations

- Units on the controller host: `wrangl-controller.service` and `cloudflared.service`.
- Back up on a timer (`wrangl controller backup`) to S3. Litestream is optional.
- **On restart:** runners buffer through the reconnect, and JetStream holds their events. Any
  runner whose heartbeat is older than 60 s is `offline`, and its run fails after 10 min unless
  the runner returns. When a run fails, its `in_progress` leaves revert to `open`.
- **AWS:** see `docs/wrangl/aws-iam.md` for the controller and runner roles, the security
  group and teardown.

## 14. Verified so far

On 2026-10-31, in a scratch module with `modernc.org/sqlite` and `CGO_ENABLED=0`, goose and
sqlc 1.31.1, the full schema migrates up and down. Checked:

- foreign keys, CHECK constraints and FTS5 search
- sqid immutability trigger
- sibling and root rank uniqueness
- same-project parent and precursor triggers
- subtree cascade delete
- depth-first rank ordering of a subtree
- `NextReadyLeaf` skipping precursor-blocked leaves, with same-run satisfaction versus another
  run, and merge-based satisfaction
- precursor cycle detection, ancestor detection, subtree locking

**sqlc quirks** found, with workarounds in the M1 plan:
1. `WHERE <fts_table> MATCH ?` is rejected. Use the column form instead.
2. A recursive CTE referenced only inside `EXISTS` in a FROM-less SELECT is unresolved. Put the
   CTE in FROM and use `COUNT(*) > 0`.
3. A parameter compared to a CTE column needs an explicit `CAST(sqlc.arg(x) AS INTEGER)`.
4. A bare `rank` column is parsed as the `RANK()` keyword. Always qualify it (`task.rank`).
5. Never mix bare `?` with `sqlc.arg()`/`sqlc.narg()` in one query. sqlc emits `?N` for the named
   parameters while SQLite keeps counting the bare ones, so arguments shift. Found in M2.
6. `sqlc.arg()` inside `CASE WHEN … IN (…)` is left unrewritten, so the literal text reaches
   SQLite (a syntax error at runtime). Compute such values in Go. Found in M4.
7. A parameter sqlc can't type becomes `interface{}`. Left at its zero value it's `NULL`, and
   `NULL = ''` never matches, so optional filters silently return nothing. `CAST` every such
   parameter. Found in M4.

`db_test.go` guards quirks 6 and 7 by scanning the generated code: it fails on any unrewritten
`sqlc.*()` macro and on any `interface{}` parameter.

## 15. Milestones

| M | Scope | Depends |
|---|---|---|
| **M1 ✅** | Cobra tree (stubs), config, `db` (full schema + hierarchy queries), `stack` primitives (sqids, fractional ranks), `controller init/serve/migrate/secret` skeleton with embedded NATS + healthz | — |
| **M2 ✅** | NATS operator/JWT + TLS, enrolment, `runner bootstrap/run/status`, heartbeat, offline sweep, `fleet list/show/enroll-token`; **pulled forward from M3:** templates create/list/show/delete, EC2 provisioner with tagging, `fleet launch/terminate/teardown`, 30-min idle reaper, orphan poweroff | M1 |
| **M3 ✅** | `agent` user + root daemon (D26), template apply on runners (mise) + `template edit` (versioning, failure handling), scheduler scale-up with backoff, `fleet cordon/uncordon/drain` | M2 |
| **M4 ✅** | Projects/repos/issues, **work-stack CRUD via `stack` + CLI** (`task tree/create/move/needs/split/queue/…`), GitHub App client (author tokens; tested against a fake), assignment, headless work loop in a PTY as `agent`, checkpoint commits + push, ingest/logs/FTS, cancel/attach/diff/files/cat, install guide (`docs/wrangl/INSTALL.md`) | M3 |
| **M5 ✅** | Agent socket (peer-uid auth), `runner hook/mcp/tool`, dependency-free MCP core, stack tools with subtree scope + approvals, Questions (agent/system/approval), `task_complete`-driven completion with one nudge then an owner question | M4 |
| **M6 ✅** | PR open (by the controller), gates (CI, size, protected globs), review runs + `submit_review`, reviewer-App approve/squash-merge, `pr_review` questions, revise runs on the PR branch, HMAC webhooks with dedupe + 15-min reconcile, blocked continuation, `pr` CLI | M5 |
| **M7 ✅** | In-process event bus, `GET /api/v1/events` SSE (15 s keepalive), PTY as SSE with `Last-Event-ID` resume, `wrangl tui` (bubbletea: runners, runs, stack editing, questions, PRs; attach/logs/diff hand over the terminal) | M4 |
| **M8 ✅** | PWA: nav + Inbox, **Backlog editor (drag/drop, new task, split, split with agent + `plan` runs)**, **Runners view** (cards, gauges, detail), runs/diff/file/markdown, xterm, web push, CF Access verify | M7 |
| **M9 ✅** | Experimental: interactive drive + Remote Control prototype; Codex/Antigravity templates | M5 |
| **M10 ✅** | v2: controller-hosted remote MCP endpoint exposing the stack tools to the owner's own agents (auth design behind CF Access) | M5, M8 |

## 16. Decisions log

| # | Decision |
|---|---|
| D1 | All Go, one binary with modes; NATS embedded in controller; SQLite + sqlc (`modernc`, pure Go) |
| D2 | Issue = GitHub issue reference |
| D3 | Controller launches EC2, choosing instance types to meet template minimums |
| D4 | One assignment per runner; one subtree never split across runners |
| D5 | Generic agents via template commands; hooks fire back over NATS |
| D6 | Read-only PTY; human input via Questions |
| D7 | Logs: JetStream for live tail, SQLite FTS5 for search |
| D8 | Diffs live from the runner while active, else from the PR |
| D9 | Two GitHub Apps (author, reviewer); review runs execute on a runner |
| D10 | Datastar + templ + goldmark; single user; client API is HTTP/2 + SSE |
| D11 | Controller is a small EC2 host in the runners' VPC/SG; keep Cloudflare ZT; GitHub webhooks via Access bypass path + HMAC |
| D12 | Web push in v1 |
| D13 | Name: **wrangl**; same repo as `ssm` |
| D14 | Agent auth: `claude setup-token` token held by controller, injected per run; Remote Control optional/experimental (M9) |
| D15 | Hierarchical tasks, ranked within parent, immutable sqids, same-project precursors; parents are containers |
| D16 | Blocked work: skip to next ready leaf; if none, release runner, run → `blocked`, auto-continue when precursors complete |
| D17 | One branch + one PR per assignment, checkpoint commit per subtask; one agent session per assignment |
| D18 | Assignment ends when no open leaves remain; agents read project-wide, write within their subtree; deletes/cancels of others' tasks need approval |
| D19 | Stack tools also exposed to the owner's agents (v2, M10) |
| D20 | PWA: bottom-tab nav with Inbox; Backlog editor (drag/drop, new task, split); Runners view |
| D21 | Split with agent ships in M8: `plan` run kind, read-only stack scope + `propose_split`, accepted from Inbox via the atomic split |
| D22 | Every EC2 resource tagged (`wrangl:managed`, `wrangl:controller`, `wrangl:runner`, `wrangl:template`, `Name`); terminate re-checks tags; `fleet teardown` works directly against AWS |
| D23 | Controller-launched runners idle (or offline) for 30 min are terminated; template `idle_timeout` overrides |
| D24 | Controller uses the instance role (default credential chain); no AWS keys in config |
| D25 | Enrolment proof for launched runners = token bound to instance id + source IP equals its private IP (replaces identity-document signature check) |
| D27 | M5: the stuck-task question offers Retry / Mark complete / Fail run ("skip" would hand the same leaf back). Approvals apply inside the assignment; outside it the agent can only read. Approval questions outlive the run; agent/system questions are cancelled with it |
| D28 | M6: the controller (author App) opens PRs after a push, so GitHub's API is only ever called from the controller. Without a reviewer App PRs are opened but not reviewed/merged by wrangl. One review run per PR head; a review run that submits nothing goes to the owner |
| D29 | M8: the PWA uses stdlib `html/template` instead of templ (no code generation step, no extra tool in the gate) and vendors Datastar 1.0.0-beta.11, SortableJS and xterm.js into the binary instead of a CDN. `/ui/*` is the PWA's own action surface; `/api/v1` stays the stable client API |
| D30 | M9: Remote Control is not a switch but an interactive-drive template: `drive: interactive` + `secret_files` (full login, 0600, deleted at run end) + `expect` rules for first-run prompts + the agent's own flag in `start`. Example templates for Claude (headless, interactive) and Codex ship in `docs/wrangl/templates/`, the interactive and Codex ones marked unverified |
| D31 | M10: the remote MCP endpoint reuses the API's exposure and auth (unix socket; TCP behind Cloudflare Access, verified in-process when configured) rather than a separate token scheme; `wrangl mcp` bridges stdio so agents need no Access headers of their own. The owner scope may cancel/delete without approval (the owner *is* the approver) |
| D32 | Agent plug-ins: hook formats `--continue-format claude|codex|agy|gemini|text`; agents that can't take a preset session id are resumed by `{{.AgentSessionID}}`, read from their JSON output (preferred) or hook payloads; `files: merge: json` pre-accepts agents' own prompts (Claude trust/bypass) instead of PTY regexes. Auth policy per agent in `docs/wrangl/agents.md`: env tokens over login files, never one login file on concurrent runners |
| D33 | Task type is an enum (`code`, `ux`, `architecture`, `spec`, `docs`, `review`): TEXT + CHECK in SQLite, mapped by a sqlc column override onto `dbtypes.TaskType`; inherited by subtasks; adds type guidance to agent prompts; shown as a shape icon (status keeps colour) |
| D34 | Runners self-upgrade in place over NATS (`cmd.upgrade` carries only the expected sha256; the runner fetches from its enrolment URL, verifies, renames and re-execs), gated by an upgrade mark that blocks new claims; not SSM, not relaunch |
| D26 | Runner daemon runs as root and holds the NATS creds; the agent (and its mise toolchain, hooks, MCP shim) runs as the unprivileged `agent` user with no access to them |

## 17. Open items

None right now.

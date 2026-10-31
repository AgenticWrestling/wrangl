# dh — Controller / Runner Design

**Date:** 2026-10-31
**Status:** Draft — awaiting owner review (see §15 Open questions)

## 1. Overview

`dh` is a single Go binary that runs in one of two **modes**, plus a set of **client**
commands:

- **Controller** — runs on a small dedicated EC2 host. Owns the SQLite database, an embedded
  NATS server with JetStream, the EC2 provisioner, the scheduler, the GitHub integration
  (PR review and merge), the HTTP API, and the mobile PWA served through Cloudflare Zero Trust.
- **Runner** — runs on EC2 instances that the controller launches. It enrols itself, applies
  its RunnerTemplate (a mise config plus a setup script), and supervises one coding-agent run at
  a time in a PTY. It streams output and agent hook events back over NATS, then pushes a branch
  and opens a PR.
- **Client** — CLI and TUI commands (`dh project …`, `dh task …`, `dh tui`, …) that talk to the
  controller's HTTP/2 + SSE API. The PWA uses that same API surface.

It lives in the `devhost` repo next to `ssm`, as `cmd/dh`.

### Goals

- Set up tooling on remote machines declaratively, including coding agents, from a RunnerTemplate.
- Have the controller launch EC2 capacity that meets each template's minimum hardware spec,
  and assign work to it.
- Show everything running and queued across all runners: TUI, CLI and mobile PWA.
- Cancel runs, watch read-only terminal output live, and search/filter logs.
- Manage projects, link repos, import GitHub issues and create tasks.
- PR flow: the runner pushes a PR, the controller reviews it, auto-merges low-risk changes,
  and asks the owner to review everything else, with a web push notification.
- Support any agent, wired in through hooks that fire back over NATS.

### Non-goals (v1)

- Multiple users or RBAC. There is one owner, and Cloudflare Access is the identity.
- Running more than one task at a time on a runner (the schema allows it later; §6).
- Interactive terminal input. The PTY view is read-only, and agents ask for human input
  through **Questions**.
- Non-GitHub forges, non-Linux runners, and multi-region fleets.
- High availability for the controller. It is a single process; restore from backup (§13).

## 2. Topology

```
                         Cloudflare Zero Trust (Access)
   phone (PWA) / laptop  ───────────────▶ cloudflared (on controller host, outbound only)
                                                 │ http://127.0.0.1:8080
┌──────────────────────── VPC / shared SG ───────┼─────────────────────────────────┐
│  ┌──────────────── controller host (small EC2) ▼───────────────┐                  │
│  │ dh controller serve                                          │                  │
│  │  ├─ HTTP API + SSE + PWA        127.0.0.1:8080 (+ unix sock) │                  │
│  │  ├─ enrol listener               <private-ip>:4223           │◀── enrol, binary │
│  │  ├─ embedded NATS + JetStream    <private-ip>:4222           │◀── runners (TLS) │
│  │  ├─ SQLite (WAL) + sqlc          /var/lib/dh/dh.db           │                  │
│  │  ├─ scheduler · provisioner (EC2 API, instance role)         │                  │
│  │  └─ GitHub App client · reviewer · web push sender           │──▶ GitHub, push  │
│  └──────────────────────────────────────────────────────────────┘    services      │
│        ▲ NATS                         ▲ NATS                         (via NAT)     │
│  ┌─────┴──────────┐           ┌───────┴────────┐                                   │
│  │ runner (EC2)   │    …      │ runner (EC2)   │   dh runner run (systemd, user dh)│
│  │  PTY ▸ agent   │           │  PTY ▸ agent   │   agent ⇄ dh runner mcp (stdio)   │
│  │  hooks ▸ dh runner hook    │                │   hooks ⇄ dh runner hook          │
│  └────────────────┘           └────────────────┘                                   │
└────────────────────────────────────────────────────────────────────────────────────┘
```

- Nothing listens on the internet. The UI and API are reachable only through `cloudflared`.
- The SG references itself, so runners can reach the controller on 4222 (NATS) and 4223
  (enrolment and binary download). Runners expose no ports at all.
- Runners need outbound internet access (mise downloads, GitHub, the model provider). The
  controller needs outbound access for GitHub, web push and the EC2 API.

## 3. Command tree (cobra)

```
dh
├── controller                          # ── mode: runs on the controller host
│   ├── init      [--data-dir /var/lib/dh] [--config /etc/dh/controller.yaml]
│   │             # writes a config skeleton; generates the NATS operator/account keys,
│   │             # VAPID keys and the TLS cert for 4222; creates the DB and runs migrations
│   ├── serve     [--config] [--data-dir]
│   │             # SQLite + embedded NATS + scheduler + provisioner + HTTP (one process)
│   ├── migrate   [up|down|status]
│   └── backup    <path>                # consistent online SQLite backup (VACUUM INTO)
│
├── runner                              # ── mode: runs on a runner instance
│   ├── bootstrap --controller <http://ip:4223> --token <dhe_…> [--sha256 <bin-hash>]
│   │             # run by cloud-init as root: creates user `dh`, generates the NKey,
│   │             # enrols, writes /etc/dh/runner.{yaml,creds}, installs + starts the systemd unit
│   ├── run       # the daemon (systemd ExecStart). Heartbeat, apply template, supervise runs
│   ├── hook      <event>               # agent hook entrypoint: stdin JSON → daemon → NATS
│   ├── mcp       # stdio MCP server the agent spawns; proxies to the daemon (unix socket)
│   └── status    # local: identity, NATS link, template version, current run
│
│   # ── client commands (HTTP API; also what the TUI/PWA call) ──────────────────────
├── tui                                 # full-screen dashboard (§10)
├── project   create <slug> [--name --template --auto-merge] | list | show | edit | delete
├── repo      link <project> <owner/name> [--branch] | unlink | list [--project]
├── issue     sync <project> | list [--project --state] | show | to-task <issue> [--template]
├── task      create <project> --title [--prompt | -f prompt.md] [--repo --template --issue --priority]
│             list [--project --state] | show | cancel | retry
├── run       list [--state --runner --project] | show | cancel
│             attach <run>              # read-only live PTY (replays from start, then follows)
│             diff <run> [--stat]       # live from the runner while active, else from the PR
│             files <run> [path] | cat <run> <path>
├── logs      <run> [-f] [--grep RE] [--stream agent|runner|hook|system] [--since]
│             search <fts-query> [--project --runner --since --limit]
├── pr        list [--state] | show <pr> | approve <pr> | request-changes <pr> -m … | close <pr>
├── question  list [--open] | show <id> | answer <id> (<option-value> | --text …)
├── template  create -f template.yaml | list | show | edit <name> | delete
├── fleet     list | show <runner> | launch --template <name> [--count]
│             cordon | uncordon | drain | terminate <runner>
│             enroll-token --template <name> [--ttl 30m]   # adopt a box the controller didn't launch
├── push      test                      # send a test web push to every subscription
└── config    use <url> | show          # client context (~/.config/dh/config.yaml)

global flags:  --controller <url|unix:///run/dh/api.sock>   -o table|json|yaml   --config
```

**Naming.** `dh runner …` means *being* a runner, and `dh fleet …` means *managing* runners.
Keeping the two apart prevents `dh runner list` from being ambiguous.

**Client transport.** On the controller host the client talks to `unix:///run/dh/api.sock`,
which is trusted through file permissions (`dh` group). Anywhere else it talks to
`https://<cf-hostname>` and sends a Cloudflare Access **service token**
(`CF-Access-Client-Id/Secret`, stored in the client config). The controller checks the
resulting `Cf-Access-Jwt-Assertion` the same way it does for the browser (§11).

## 4. Package layout

```
cmd/dh/
  main.go                       root cmd, global flags, mode/client registration
  internal/
    cli/                        cobra commands, one file per noun (project.go, task.go, …)
    client/                     typed HTTP API client + SSE reader (used by cli, tui)
    api/                        HTTP handlers (JSON + SSE), request/response types, CF auth
    web/                        Datastar views (templ), static assets, manifest, sw.js
    tui/                        bubbletea dashboard
    db/
      migrations/*.sql          goose, embedded
      queries/*.sql             sqlc input
      sqlc.yaml
      dbgen/                    sqlc output (committed)
      db.go                     Open(): modernc.org/sqlite, WAL, pragmas, migrate
    controller/
      server.go                 wires everything; lifecycle
      natsd/                    embedded server, operator/account JWTs, user JWT issuance
      enroll/                   :4223 listener — /enroll, /dl/dh-linux-{arch}
      scheduler/                assigns queued runs, scale-up/idle-reap decisions
      provision/                EC2 RunInstances, instance-type selection, user-data
      ingest/                   JetStream consumers → SQLite (state, logs, hooks, questions)
      gh/                       GitHub App: tokens, PRs, checks, issues, reviews, merge
      review/                   risk gates + review-run orchestration + decision
      webpush/                  VAPID, subscriptions, send
    runner/
      bootstrap/                user, nkey, enrol, systemd unit
      daemon/                   NATS link, heartbeat, command handlers, local unix socket
      template/                 apply mise.toml + setup script; idempotent reconcile
      workspace/                clone/worktree, branch, commit detection, push, open PR
      pty/                      spawn agent in PTY, fan bytes to NATS + local file
      hooks/                    `dh runner hook` → daemon
      mcp/                      stdio MCP server (modelcontextprotocol/go-sdk)
    proto/                      NATS subjects + message structs shared by both modes
```

## 5. Data model (SQLite, sqlc)

- Driver: `modernc.org/sqlite` (pure Go, `CGO_ENABLED=0`). Migrations: `goose`, embedded
  and run on `serve` start. Queries: `sqlc` (sqlite engine, `database/sql`).
- Pragmas: `journal_mode=WAL`, `foreign_keys=ON`, `busy_timeout=5000`, `synchronous=NORMAL`.
  One writer pool (`MaxOpenConns=1`) and a separate read pool.
- Timestamps are `TEXT` in RFC 3339 UTC. Booleans are `INTEGER` 0/1. Structured blobs are `TEXT` JSON.
- Secrets (NATS seeds, VAPID private key, GitHub App private keys) live in files under the data
  dir with mode 0600, **never** in the DB.

| Table | Purpose / key columns |
|---|---|
| `runner_template` | `name`, `mise_toml`, `setup_script`, `agent_json` (§7), `min_vcpu`, `min_ram_mib`, `min_disk_gib`, `arch` (`arm64`/`x86_64`), `instance_types` (JSON preference allowlist, empty = auto), `ami` (SSM param path or AMI id), `max_runners`, `idle_timeout_s`, `version` (bumped on every edit) |
| `enrollment_token` | `token_hash` (sha256), `template_id`, `instance_id` (bound after RunInstances), `expires_at`, `used_at`, `runner_id` |
| `runner` | `name`, `template_id`, `template_version` (applied), `instance_id`, `instance_type`, `az`, `private_ip`, `arch`, `vcpu`, `ram_mib`, `disk_gib`, `nkey_public` (null until enrolled), `state`, `max_concurrent` (1), `last_heartbeat_at`, `last_metrics_json`, `terminated_at` |
| `project` | `slug`, `name`, `description`, `default_template_id`, `auto_merge` (bool), `protected_paths` (JSON globs), `max_auto_merge_lines` |
| `repo` | `project_id`, `owner`, `name`, `default_branch`, `gh_installation_id` |
| `issue` | `repo_id`, `gh_number`, `title`, `body`, `state`, `url`, `synced_at` |
| `task` | `project_id`, `repo_id`, `issue_id?`, `title`, `prompt`, `template_id`, `priority`, `state` |
| `run` | `task_id`, `runner_id?`, `kind` (`work`/`review`/`revise`), `attempt`, `state`, `branch`, `base_sha`, `head_sha`, `exit_code`, `error`, `queued_at`, `started_at`, `finished_at` |
| `pull_request` | `task_id`, `repo_id`, `run_id`, `gh_number`, `url`, `head_branch`, `state`, `risk`, `review_summary_md`, `gates_json`, `decision` |
| `log_line` + `log_line_fts` | `run_id`, `ts`, `stream` (`agent`/`runner`/`hook`/`system`), `level`, `message`; FTS5 external-content index on `message` with triggers |
| `hook_event` | `run_id`, `ts`, `name`, `payload_json` |
| `question` | `task_id`, `run_id?`, `kind` (`agent`/`pr_review`/`system`), `prompt`, `detail_md`, `allow_free_text`, `state`, `answer_option_id?`, `answer_text`, `answered_at` |
| `question_option` | `question_id`, `position`, `label`, `value`, `description` |
| `push_subscription` | `endpoint` (unique), `p256dh`, `auth`, `user_agent`, `failures`, `last_success_at` |

### State machines

```
runner:  launching → enrolling → provisioning → idle ⇄ busy
                                      ↓           ↓
         cordoned (no new work) · draining (finish, then terminate) · offline (heartbeat lost)
         → terminated

task:    open → in_progress → in_review → done
                     ↘ cancelled          ↘ (changes requested → in_progress)

run:     queued → assigned → preparing → running ⇄ needs_input → pushing → succeeded
           ↘ cancelled   (any non-terminal) → cancelling → cancelled
           ↘ failed      (agent exit ≠ 0, runner lost, timeout, push failure)
```

The controller owns every state transition. A runner *reports* facts (started, exited 0,
pushed SHA) and the controller decides what state follows. The DB is the source of truth,
and NATS is only transport.

## 6. Scheduling and provisioning

**Scheduling** happens in the controller, so it's a push model rather than a JetStream work
queue. The controller already owns the full picture (templates, capacity, launches), so a
central scheduler is simpler and avoids redelivering non-idempotent agent work.

Loop, triggered by events plus a 10 s tick:
1. Take queued runs in order of `priority DESC, queued_at`.
2. For each one, pick an `idle` runner with a matching `template_id` and the current
   `template_version`, then send `cmd.assign` (request/reply). If the runner acks, the run
   moves to `assigned`. If it times out, the run goes back to `queued`.
3. If no runner matches, and that template has fewer than `max_runners` runners counting
   ones still `launching`, call `provision.Launch(template)`.
4. Runners `idle` for longer than `idle_timeout_s` are terminated. `draining` runners are
   terminated once they go idle.

**Instance-type selection** (`provision`):
- If `instance_types` is set, take the first type in the list that meets the minimums and
  is offered in the subnet's AZ.
- Otherwise call `DescribeInstanceTypes`, filtered by `arch`, vCPU ≥ min, memory ≥ min and
  current generation, and pick the smallest by (vCPU, memory). Price lookup is out of scope
  for v1; the allowlist is how you steer cost.
- Disk: the root EBS gp3 volume is sized at `min_disk_gib`.

**Launch:**
1. Mint an enrolment token (TTL 30 min, single use).
2. Call `RunInstances` with the AMI, the type, the subnet and SG from the controller config,
   the runner instance profile, IMDSv2 required (hop limit 1), tags
   `dh:managed=true, dh:runner=<name>, dh:template=<name>`, and user-data (below).
3. Bind the token to the returned `instance_id` and insert a `runner` row in state `launching`.

```bash
#!/bin/bash
set -euo pipefail
ARCH=$(uname -m)   # aarch64|x86_64
curl -fsSL "http://10.0.1.5:4223/dl/dh-linux-${ARCH}" -o /usr/local/bin/dh
echo "<sha256>  /usr/local/bin/dh" | sha256sum -c -
chmod 0755 /usr/local/bin/dh
exec /usr/local/bin/dh runner bootstrap --controller http://10.0.1.5:4223 --token dhe_…
```

The controller serves runner binaries for both architectures from `<data-dir>/bin/`; the
build cross-compiles them (`task build`). The user-data pins their hashes.

**Controller IAM:** `ec2:RunInstances`, `ec2:CreateTags`, `ec2:Describe*`,
`ec2:TerminateInstances` (condition: `aws:ResourceTag/dh:managed=true`), and `iam:PassRole`
for the runner instance profile only. The **runner instance profile** gets
`AmazonSSMManagedInstanceCore` (so `ssm connect` works for debugging) plus model-provider
access if that is Bedrock (§15 Q3).

## 7. Enrolment and NATS security

**NATS in operator/JWT mode.** On `init`, the controller generates an operator, a `SYS`
account and a `DH` account with a signing key. Runner users are JWTs signed by the `DH`
signing key, so adding a runner needs **no server reload**. The controller's own connection
is in-process (`nats.InProcessServer`).

**Enrolment** (the runner creates the key; the private seed never leaves the box):
1. `bootstrap` creates the system user `dh` and generates an NKey user pair under `/etc/dh/`.
2. It reads the `instance_id` and the signed identity document from IMDSv2.
3. It sends `POST :4223/enroll {token, nkey_public, instance_id, identity_doc, signature, facts{vcpu, ram, disk, arch}}`.
4. The controller checks the token: hash match, unused, unexpired, bound to the same
   `instance_id`. It verifies the identity document signature (AWS public cert for the region)
   and checks the facts against the template minimums. It then burns the token, sets
   `runner.nkey_public` and moves the runner to `enrolling`. It returns
   `{runner_id, user_jwt, nats_url, nats_ca}`.
5. `bootstrap` writes `/etc/dh/runner.creds` (JWT + seed, 0600, owned by `dh`) and
   `/etc/dh/runner.yaml`, installs `dh-runner.service` (`User=dh`) and runs `enable --now`.
6. The daemon connects and requests its template (`cmd` reply), then applies it (§8). The
   runner moves to `provisioning`, then to `idle`.

A token in user-data is readable by any process on the box through IMDS, which is
acceptable: it is single-use, short-lived, bound to that instance id, and already burned by
the time any agent runs.

**Subjects and per-runner permissions** (`<r>` = runner id, `<x>` = run id):

| Subject | Dir | Kind | Purpose |
|---|---|---|---|
| `dh.r.<r>.cmd.{assign,cancel,template,answer,diff,tree,file}` | C→R | request/reply | control and inspection |
| `dh.r.<r>.hb` | R→C | core | heartbeat every 10 s + metrics (gopsutil: CPU, mem, disk %) |
| `dh.evt.<r>.<x>.{state,log,hook,question}` | R→C | JetStream `DH_EVT` (file, 7 d) | durable events; ingested to SQLite |
| `dh.pty.<r>.<x>` | R→C | JetStream `DH_PTY` (file, 2 GiB / 3 d) | raw PTY bytes, live tail and replay |
| `dh.evt.<r>.template` | R→C | JetStream `DH_EVT` | template apply progress/result |

Runner `<r>` may **subscribe** only to `dh.r.<r>.cmd.>` and `_INBOX_<r>.>`, and may
**publish** only to `dh.evt.<r>.>`, `dh.pty.<r>.>`, `dh.r.<r>.hb`, plus the JetStream API
subjects needed for publish acks. It uses the custom inbox prefix `_INBOX_<r>`. TLS on 4222
uses a cert generated at `init`; runners pin `nats_ca` from the enrolment response.

Events go through JetStream so nothing is lost while the controller restarts. `ingest`
consumers use durable consumers, and writes are idempotent (keyed on stream sequence).

## 8. Runner: templates, agents, hooks

**RunnerTemplate file** (`dh template create -f`):

```yaml
name: go-claude
arch: arm64
min: { vcpu: 4, ram: 16GiB, disk: 100GiB }
instance_types: [m7g.xlarge, m7g.2xlarge]     # optional preference list
max_runners: 3
idle_timeout: 30m
mise_toml: |
  [tools]
  go = "1.26"
  node = "24"
  "npm:@anthropic-ai/claude-code" = "latest"
  gh = "latest"
setup_script: |
  sudo dnf install -y git tmux
agent:
  command: ["claude", "-p", "{{.Prompt}}", "--output-format", "stream-json", "--verbose"]
  env: { CLAUDE_CODE_USE_BEDROCK: "1" }
  timeout: 4h
  files:                       # written into the worktree / $HOME before launch; templated
    - path: "{{.Workdir}}/.claude/settings.local.json"
      content: |
        {"hooks": {
          "PostToolUse":  [{"hooks":[{"type":"command","command":"dh runner hook PostToolUse"}]}],
          "Notification": [{"hooks":[{"type":"command","command":"dh runner hook Notification"}]}],
          "Stop":         [{"hooks":[{"type":"command","command":"dh runner hook Stop"}]}]},
         "permissions": {"defaultMode": "bypassPermissions"}}
    - path: "{{.Workdir}}/.mcp.json"
      content: '{"mcpServers":{"dh":{"command":"dh","args":["runner","mcp"]}}}'
```

Agents stay generic: `dh` knows nothing about Claude Code specifically. A template supplies
the command, the env, the config files that register `dh runner hook <event>` and
`dh runner mcp`, and a timeout. Template variables are `Prompt`, `Workdir`, `RunID`,
`TaskID`, `Branch` and `Repo`.

**Applying a template** is idempotent: write `~dh/.config/mise/config.toml`, run
`mise install`, then run `setup_script` if its hash changed. Progress goes to
`dh.evt.<r>.template`. If you edit a template, the version bumps, and idle runners are sent
`cmd.template` and re-apply it. Busy runners re-apply after their current run.

**Run lifecycle on the runner:**
1. `cmd.assign` brings the run id, the task prompt, the repo, the base branch and a **GitHub
   App installation token** minted for this run, scoped to the one repo
   (`contents:write, pull_requests:write`). The token is never written to disk; git gets it
   through a credential helper served by the daemon.
2. **Prepare:** a cached bare clone at `~dh/repos/<owner>/<name>.git`, then a worktree at
   `~dh/work/<run>`, then branch `dh/<task>-<attempt>`. Write the agent files.
3. **Run:** spawn the agent in a PTY (`creack/pty`, 200×50) with env `DH_RUN_ID` and
   `DH_SOCK=/run/dh/runner.sock`. PTY bytes go to `dh.pty.…` and to
   `~dh/work/<run>.pty`. Lines are also split out (ANSI stripped) to `dh.evt.….log`.
4. **Hooks:** `dh runner hook <event>` reads stdin JSON and posts `{run, event, payload}` to
   the daemon socket, which publishes `dh.evt.….hook`. It exits 0 immediately and never
   blocks the agent.
5. **MCP tools** (`dh runner mcp`, stdio, proxied over the daemon socket):
   - `ask_question(prompt, options[{label,value,description}], allow_free_text, detail_md)`
     blocks until the question is answered or cancelled. The run is `needs_input` meanwhile.
   - `report_status(summary, percent?)` shows up in the TUI/PWA.
   - `get_task()` returns the title, prompt, issue body and links.
6. **Cancel:** `cmd.cancel` sends SIGTERM to the PTY process group, then SIGKILL after 15 s.
7. **Finish:** if the agent exits 0 and there are commits ahead of base, the runner pushes and
   opens a PR (title and body from the task plus the agent's final status), and reports
   `pushing → succeeded` with `head_sha` and the PR number. If there are no commits, the run
   succeeds with a note and the task goes back to `open`.
8. **Inspection** while active: `cmd.diff` (`git diff base...HEAD` plus the working tree),
   `cmd.tree` and `cmd.file`, with responses capped at 4 MiB.
9. Worktrees are removed 24 h after the run, or sooner when disk passes 85 %.

## 9. PR review and merge

When a `work` run succeeds with a PR:
1. **Deterministic gates**, computed by the controller from the GitHub API:
   - CI check runs are all green (polled until done or 60 min)
   - diff size ≤ `project.max_auto_merge_lines`
   - no changed path matches `project.protected_paths` (default: `.github/**`, `**/migrations/**`,
     `go.mod`, `**/*.tf`, `Dockerfile*`)
   - no changes to binaries or lockfiles beyond configurable limits
2. **Review run:** the controller queues a `review` run on the reviewer template, which
   defaults to the work template. Its prompt includes the PR diff, and it must call MCP
   `submit_review(risk: low|medium|high, summary_md, concerns[])`. That tool exists only in
   review runs.
3. **Decision:**
   - **Auto-merge** happens when `project.auto_merge` is on, every gate passes, and risk is
     `low`. The PR is approved and squash-merged, and the task becomes `done`.
   - **Otherwise** a `pr_review` **Question** is created with the review summary, the gate
     results and a link to the diff view. Options: *Approve & merge* · *Request changes*
     (free text) · *Close*. A web push goes out.
   - *Request changes* queues a `revise` run on the same branch, with the review notes and
     your text appended to the prompt. That run pushes to the same PR, and the cycle repeats.
4. GitHub state (checks, PR merged or closed outside dh) is **polled** every 60 s for open
   PRs, and issues sync on demand. Webhooks are deferred (§15 Q4).

## 10. API, SSE, TUI

- **API:** JSON under `/api/v1/…`, mirroring the CLI nouns. The CLI, TUI and PWA all use it.
  Served as h2c on the loopback/unix socket; Cloudflare gives browsers HTTP/2 or 3 at the edge.
- **SSE:**
  - `/api/v1/events` is the global feed of runner, run, question and PR changes.
  - `/api/v1/runs/{id}/pty` sends base64 chunks from JetStream `DH_PTY`. It replays from
    sequence 0 or from `Last-Event-ID`, then follows.
  - `/api/v1/runs/{id}/logs?grep=` streams log lines.
  - Every stream sends a keepalive comment every 15 s, which keeps it alive through
    Cloudflare's idle timeout.
- **TUI** (`bubbletea` + `lipgloss`):
  - Panes: **Runners** (state, template, CPU/mem/disk, current run), **Runs** (running,
    needs_input and queued, plus recently finished), **Questions** and **PRs**.
  - Keys: `enter` details · `a` attach PTY (rendered through a vt emulator, read-only) ·
    `l` logs with `/` filter · `d` diff · `c` cancel · `n` new task · `p` projects ·
    `r` link repo · `q` answer a question.
  - It drives everything through one `/api/v1/events` stream, plus per-run streams when you
    attach.

## 11. Web PWA (Datastar)

- Server-rendered with `templ` and the Datastar Go SDK. SSE pushes fragment and signal
  patches. Mobile-first layout, installable PWA (manifest + service worker).
- **Views:** dashboard (runners, active and queued runs, open questions) · project → repos,
  tasks, issues · task → runs, PR · run → live terminal (xterm.js, read-only), logs with
  search/filter, hook timeline · **diff** (server-parsed unified diff, per-file collapsible,
  `chroma` highlighting, split or unified view) · **file browser** (tree plus contents, chroma)
  with **markdown preview** (`goldmark` + GFM, sanitised with `bluemonday`) · question answer
  sheet · PR review (summary, gates, diff, actions) · create project/task/link repo forms.
- **Web push (v1):**
  - VAPID keys are generated at `init`. The browser subscribes through `/api/v1/push/subscribe`.
  - Library: `SherClockHolmes/webpush-go`.
  - Events that notify: question created, PR awaiting review, run failed, runner offline.
  - Notification click deep-links to the item. On Android, notification actions (Approve,
    Close) post from the service worker.
  - iOS needs the PWA installed to the home screen (16.4+), and has no actions.
  - Subscriptions that return 404 or 410 are deleted.
- **Cloudflare Access specifics:**
  - The controller verifies `Cf-Access-Jwt-Assertion` (RS256 against
    `https://<team>.cloudflareaccess.com/cdn-cgi/access/certs`, cached) and checks `aud` and
    the owner email from config. It does not trust the tunnel alone.
  - `<link rel="manifest" crossorigin="use-credentials">`. Without it, Access blocks the
    manifest fetch and installing the PWA fails.
  - Set a long Access session duration for the app, because re-auth inside an installed iOS
    PWA is awkward.

## 12. Configuration

`/etc/dh/controller.yaml`:

```yaml
data_dir: /var/lib/dh
listen: { http: 127.0.0.1:8080, unix: /run/dh/api.sock, nats: 10.0.1.5:4222, enroll: 10.0.1.5:4223 }
aws: { region: us-west-2, subnet_ids: [subnet-…], security_group_id: sg-…, runner_instance_profile: dh-runner }
github:
  author_app: { id: 123, private_key: /var/lib/dh/gh-author.pem }
  reviewer_app: { id: 456, private_key: /var/lib/dh/gh-reviewer.pem }   # see §15 Q1
cloudflare: { team: myteam, aud: "…", owner_email: you@example.com }
web: { public_url: https://dh.example.com }
```

The client config `~/.config/dh/config.yaml` holds `controller` (URL) and an optional
`cf_service_token {id, secret}`.

## 13. Operations

- Two systemd units on the controller host: `dh-controller.service` and `cloudflared.service`.
- **Backup:** `dh controller backup` on a timer to S3. Litestream is an optional upgrade
  (as in kj-web). JetStream data is disposable apart from PTY replay.
- **Restart behaviour:** runners keep running and buffer through the reconnect, and JetStream
  holds their events until the controller consumes them. On start the controller reconciles:
  any runner whose heartbeat is older than 60 s is `offline`. Its active run is `failed`
  after 10 min unless the runner comes back.
- **Upgrades:** replace the binary, restart the controller, and runners pick up the new binary
  on their next launch. An in-place runner self-update is deferred.

## 14. Milestones

| M | Scope | Depends |
|---|---|---|
| **M1** | Cobra tree (all commands stubbed), config loading, `db` package (goose + sqlc + full schema), `controller init/serve/migrate` skeleton with embedded NATS + health endpoint, `task` targets for sqlc/build | — |
| M2 | NATS operator/JWT, enrol listener, `runner bootstrap/run`, heartbeat, `fleet list/show`, `fleet enroll-token` | M1 |
| M3 | Templates CRUD + apply on runner, EC2 provisioner, scheduler scale-up/idle-reap, `fleet launch/cordon/drain/terminate` | M2 |
| M4 | Projects/repos/issues/tasks CRUD, GitHub App tokens, workspace + PTY run supervision, ingest (state/logs/FTS), `run cancel/attach`, `logs`, `logs search` | M3 |
| M5 | Hooks, MCP shim, Questions end-to-end | M4 |
| M6 | PR push/open, gates, review runs, auto-merge, `pr` commands, revise loop | M5 |
| M7 | HTTP API completion + SSE streams + TUI | M4 (parallel with M5/M6) |
| M8 | PWA: Datastar views, diff/file/markdown, xterm, web push, CF Access verification | M7 |

## 15. Open questions (defaults in bold)

1. **Self-approval.** GitHub won't let a PR's author approve it, and if the runner pushes as
   App X, App X can't approve. Choose between **two GitHub Apps (author + reviewer)** or one
   App that merges without an approving review, which means no "required approvals" rule in
   branch protection.
2. **Where review runs execute:** **on a runner (reviewer template)**, or on the controller host.
3. **Model credentials for agents:** **Bedrock through the runner instance role (no keys
   to distribute)**, or an API key held by the controller and injected per run.
4. **GitHub events:** **poll in v1**, or webhooks through a Cloudflare Access bypass path
   for `/gh/webhook`, verified by HMAC.
5. **Binary name:** **`dh`**.

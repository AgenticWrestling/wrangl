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
| `project` | `slug`, `name`, `default_template_id`, `auto_merge`, `protected_paths`, `max_auto_merge_lines`, `priority` (plus `auto_queue` in M4) |
| `repo` | `project_id`, `owner`, `name`, `default_branch`, `gh_installation_id` |
| `issue` | `repo_id`, `gh_number`, `title`, `body`, `state`, `url`, `synced_at` |
| `task` | `sqid` (unique, immutable), `project_id`, `parent_id`, `rank`, `repo_id?`/`template_id?` (NULL = inherit), `issue_id?`, `title`, `prompt`, `state`, `created_by`, `completion_summary`, `completed_run_id` |
| `task_dependency` | (`task_id`, `depends_on_id`): precursors, same project |
| `task_event` | audit: `task_sqid`, `run_id?`, `actor`, `op`, `diff_json`, `ts` |
| `run` | `task_id` (assignment root), `current_task_id`, `runner_id`, `kind` (`work`/`review`/`revise`), `attempt`, `state`, `branch`, `session_name`, `base_sha`, `head_sha`, `status_text`, `exit_code`, `error`, timestamps |
| `pull_request` | `task_id` (root), `repo_id`, `run_id`, `gh_number`, `url`, `head_branch`, `state`, `risk`, `review_summary_md`, `gates_json`, `decision` |
| `log_line` + `log_line_fts` | `run_id`, `seq`, `ts`, `stream`, `level`, `message`; FTS5 external-content index |
| `hook_event` | `run_id`, `seq`, `ts`, `name`, `payload_json` |
| `question` / `question_option` | `kind` (`agent`/`pr_review`/`approval`/`system`), prompt, options, answer |
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
5. Reap runners idle longer than `idle_timeout_s`, and drained runners once they go idle.
6. When a precursor becomes `done`, re-evaluate `blocked` runs (§8) and waiting queued runs.

**Instance-type selection, launch, user-data, IAM** are unchanged from rev 1:
- Use the template's `instance_types` allowlist if set, otherwise `DescribeInstanceTypes`
  filtered by arch and the minimums, smallest first.
- Root gp3 volume sized at `min_disk_gib`, IMDSv2 required, tags `wrangl:managed|runner|template`.
- User-data fetches `wrangl-linux-$(uname -m)` from `:4223`, checks its sha256, and runs
  `wrangl runner bootstrap`.
- Controller IAM: `RunInstances`, `CreateTags`, `Describe*`, `TerminateInstances` conditioned
  on `wrangl:managed=true`, and `PassRole` for the runner profile only. The runner profile gets
  `AmazonSSMManagedInstanceCore` so `ssm connect` works for debugging.

## 7. Enrolment and NATS security

Unchanged from rev 1, renamed.

- **Operator/JWT mode.** The `WRANGL` account's signing key signs runner user JWTs, so adding a
  runner needs no server reload. The controller connects in-process.
- **Enrolment steps:**
  1. The runner generates its NKey; the private seed never leaves the box.
  2. It sends `POST :4223/enroll {token, nkey_public, instance_id, identity_doc, signature, facts}`.
  3. The controller checks the token (hash, unused, unexpired, bound to that instance id),
     verifies the identity document signature, checks the facts against the template
     minimums, burns the token, and returns `{runner_id, user_jwt, nats_url, nats_ca}`.
  4. Bootstrap writes `/etc/wrangl/runner.{yaml,creds}` and installs `wrangl-runner.service`
     (`User=wrangl`).

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
- Applying a template is idempotent (mise config → `mise install` → `setup_script` if its hash
  changed). Editing a template bumps its version, and runners re-apply it when idle.
- Agents stay generic. Codex fits the same shape (`codex exec …` / `codex exec resume …`).
  Antigravity gets its own template once its headless interface is checked.

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
- With `remote_control.enabled: true`, `drive` becomes `interactive`: the agent runs
  `claude --rc "<session_name>" …` in the PTY, using a full credentials file copied from the
  controller for that run. The runner answers the first-run prompts.
- That mode is gated behind a prototype (M9). The risks to check there: several machines
  refreshing the same login can invalidate each other, and the prompt handling is undocumented.

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
4. **Interactive drive** (Remote Control) does the same loop. The `Stop` hook acts as the turn
   boundary: `wrangl runner hook Stop --continue-format claude` asks the daemon for the next
   leaf and prints Claude Code's "continue" response with the next prompt, or exits 0 to let the
   session stop. Hook output formats are verified per agent in M9.
5. **Hooks** (`wrangl runner hook <event>`): read stdin JSON and post it to the daemon socket,
   which publishes `wr.evt.….hook`. They're fire-and-forget and never block the agent, except
   for the `--continue-format` mode above.
6. **Cancel:** SIGTERM to the PTY process group, then SIGKILL after 15 s. Leaves still
   `in_progress` revert to `open`.
7. **Inspection:** `cmd.diff`, `cmd.tree` and `cmd.file`, capped at 4 MiB. Worktrees are
   removed 24 h after the run ends, or sooner when disk passes 85 %.

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

## 11. Web PWA (Datastar)

`templ` plus the Datastar Go SDK, server-rendered, with SSE patches. Mobile-first (designed at
360–430 px, works on desktop) and installable.

There are two JS dependencies beyond Datastar, both pinned and loaded from a CDN:
- **SortableJS** for touch drag-and-drop. Datastar has no drag-and-drop.
- **xterm.js** for the terminal.

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
  - **Split with agent** (proposed, not yet decided, §17): queue a `plan` run that may only use
    the stack tools to decompose the task. It makes no repo changes, and its result shows up in
    Inbox for you to accept.
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

## 15. Milestones

| M | Scope | Depends |
|---|---|---|
| **M1** | Cobra tree (stubs), config, `db` (full schema + hierarchy queries), `stack` primitives (sqids, fractional ranks), `controller init/serve/migrate/secret` skeleton with embedded NATS + healthz | — |
| M2 | NATS operator/JWT, enrolment, `runner bootstrap/run`, heartbeat, `fleet list/show/enroll-token` | M1 |
| M3 | Templates + apply, EC2 provisioner, scheduler scale-up/idle-reap, `fleet launch/cordon/drain/terminate` | M2 |
| M4 | Projects/repos/issues, **work-stack CRUD via `stack` + CLI** (`task tree/create/move/needs/queue/…`), author-App tokens, workspace, headless work loop + PTY, ingest/logs/FTS, cancel/attach | M3 |
| M5 | Hooks, MCP server with stack tools + scope/approval rules, Questions | M4 |
| M6 | PR open, gates, review runs, reviewer App approve/merge, webhooks via CF bypass, revise + blocked-continuation runs | M5 |
| M7 | HTTP API + SSE + TUI (incl. tree editing) | M4 (parallel with M5/M6) |
| M8 | PWA: nav + Inbox, **Backlog editor (drag/drop, new task, split)**, **Runners view** (cards, gauges, detail), runs/diff/file/markdown, xterm, web push, CF Access verify | M7 |
| M9 | Experimental: interactive drive + Remote Control prototype; Codex/Antigravity templates | M5 |
| M10 | v2: controller-hosted remote MCP endpoint exposing the stack tools to the owner's own agents (auth design behind CF Access) | M5, M8 |

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

## 17. Open items

1. **Split with agent:** a `plan` run kind (stack tools only, no repo changes, result accepted
   from Inbox). Worth adding to M8, or later?

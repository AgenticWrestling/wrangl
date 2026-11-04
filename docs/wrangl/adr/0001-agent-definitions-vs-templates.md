# ADR 0001: Agent definitions are separate from runner templates

- Status: **Accepted, implemented**
- Task: 3VTW9Q, under "Agent definitions; templates list the agents they allow" (fxRVRL)
- Touches: `runner_template.agent_json`, `proto.AgentSpec`, `scheduler.assignOne`, template apply / versioning (`fleet/templates_apply.go`), runner self-upgrade (`fleet/upgrade.go`), `docs/wrangl/agents.md`

## Context

A runner template today does two jobs. It describes a **machine**: arch, size, `mise_toml`,
`setup_script`, `max_runners`, `max_sessions`, idle timeout. It also embeds **one agent** in
`agent_json`: `start`/`resume` argv, `env`, `secrets`, `secret_files`, `files` (hook and MCP config,
trust pre-acceptance), `expect` rules, `drive`, `completion`, `turn_timeout`.

That coupling already hurts:

- The same agent on a different machine (Claude on a Go box, a Rust box and a design box) is
  copy-pasted into every template. `docs/wrangl/templates/` has `claude-headless.yaml` and
  `claude-interactive.yaml` that differ only in `drive` and a few files.
- Choosing an agent means choosing a machine. The owner wants a **project default agent** and a
  **per-run override** (`wrangl task queue --agent NAME`), independent of where the run lands.
- Secrets belong to the agent (Claude's `CLAUDE_CODE_OAUTH_TOKEN`, Codex's `CODEX_API_KEY`), not to
  the machine. The planned setup-token collection (riAeMu) needs "the secrets of agent X".
- Special runners (EC2 Mac, Android emulator, driven over SSH/ADB) are machines that should be
  able to host *some* agents and not others.
- Editing `agent_json` calls `UpdateTemplate`, which bumps `version`. Runners then re-run the whole
  mise install and setup script, although `agent_json` is never sent to runners with the template
  (`TemplateApply` carries only version, `mise_toml`, `setup_script`). Agent changes are resolved
  per assignment in `assignOne` and travel in the `cmd.assign` payload. Today a pure agent edit
  therefore causes a pointless machine rollout.

What is already per run, and should stay that way: the agent spec is resolved by the controller at
assignment time (secrets looked up then, names never leave the controller); `prepare()` on the
runner writes the agent's `files` into the worktree/home for each run; hook events are keyed by run
id; the PTY, logs and session ids are per run. What is per machine: the agent **binary and its
runtime** (installed by `setup_script`/`mise_toml`), and the `wrangl` binary that the hook and MCP
commands call (`wrangl runner hook`, `wrangl runner mcp`).

## Options

### A. Keep the agent inside the template; add more templates

Make a template per (machine, agent) pair; selection is choosing a template.

- Pros: no schema change; installing the agent and configuring it live in one file and one version.
- Cons: combinatorial templates; a project "default agent" or a per-run `--agent` becomes "pick a
  different template", which also changes the machine and the shared pool of runners (capacity is
  counted per template). Secrets and hook files stay duplicated. This is the status quo, extended.

### B. Separate `agent` definitions; templates list allowed agents (recommended)

A new `agent` table (named definition holding what is now `agent_json`) and a
`template_agent(template_id, agent_id)` allow-list. The project has a default agent; a run may
override it. At queue time the chosen agent must be in the allow-list of the run's template, else the run
is refused with a message naming the agent, the template and the allowed agents.

- Pros: matches the owner's decision; one definition reused across machines; agent edits do not
  bump the template version; secrets and setup tokens attach to one object; special runners just
  list the agents they support.
- Cons: a new concept, a migration, and a second thing that must agree with the template: the
  agent's binary must be installed by that template's `setup_script`. The allow-list is a
  declaration, not a guarantee.

### C. Separate agents, but no allow-list: any agent on any template

- Pros: simplest model; fewer refusals.
- Cons: nothing stops a run being sent to a machine without the binary (the failure is a
  mid-run `exec: "codex": not found`, after a runner was claimed). The owner explicitly asked for
  the refusal, and for Mac/Android templates a real constraint exists. Rejected.

### D. Agent definitions as files in the repo (per-project `.wrangl/agents/*.yaml`)

- Pros: versioned with the code; per-project customisation without controller state.
- Cons: the definition carries secret names and commands that run with the agent's authority, so a
  repo author could change what runs on the owner's runners; needs trust rules and a sync step; no
  place for the allow-list or the secret-collection UI. May be revisited as an *input* to B, not a
  replacement. Rejected for now.

## Decision

**Option B.**

### 1. Data model

```
agent            (id, name UNIQUE, description, spec_json, version, created_at, updated_at)
template_agent   (template_id, agent_id, PRIMARY KEY (template_id, agent_id))
project.default_agent_id   NULL = the only allowed agent on the run's template, else refuse
run.agent_id               the agent the run actually used (snapshot of the choice)
```

`spec_json` is today's `proto.AgentSpec` unchanged. This keeps the runner side and every existing
template YAML field valid; only where it is stored moves. `runner_template.agent_json` stays during
the transition (see Migration) and is dropped in a later release.

Template YAML gains `agents: [claude, codex]` (the allow-list). A legacy `agent:` block is still
accepted and is turned into an agent definition on create (below). New CLI: `wrangl agent
create|edit|list|show|delete`, `wrangl project edit --agent NAME`, `wrangl task queue --agent
NAME`. The web UI gets an agents list (it already has the "Add or edit an agent" block on the
inbox for the conversational agents; those are a different concept, the *controller's* agents, and
should be renamed in the UI to avoid confusion: "assistants").

### 2. Resolution and refusal

For a queued run: agent = run override, else task override (none today), else project default, else
the sole allowed agent of the resolved template, else refuse. The check "agent ∈ template's
allow-list" runs **at queue time** (so the owner sees the refusal immediately and nothing is claimed)
and again in `assignOne` (the template may have changed since). `run.agent_id` is written when the
run is queued, so later edits to the project default do not change a queued run.

### 3. Migration of `agent_json`

One goose migration, no data loss, reversible by keeping the column:

1. For each template, parse `agent_json`. Skip empty/`{}` (no agent; templates for `host` runners).
2. Name the agent from `start[0]` (`claude`, `codex`, `gemini`, `agy` becomes `antigravity`). Two
   templates with byte-identical normalised specs share one agent. Different specs for the same
   binary get a suffix from the template name, e.g. `go-claude`'s spec is `claude`; an interactive
   one is `claude-interactive` (the suffix is derived from `drive`, then the template name if still
   ambiguous). The task description's example holds: `go-claude` yields agent `claude`.
3. Insert `agent`, insert `template_agent` for the source template, set every project whose default
   template has an agent and has no default yet to that agent.
4. Leave `runner_template.agent_json` populated and read as a fallback for **one release**: if a
   template has no `template_agent` rows and a non-empty `agent_json`, behave as before. Then drop
   the column in a follow-up migration. This keeps a controller rollback safe.

The migration is a Go-assisted migration or a data step in the controller's start-up (SQL alone
cannot dedupe by normalised JSON reliably). It is idempotent: re-running finds the agents by name.

### 4. How hooks, MCP and trust files are applied per agent

Nothing about the mechanism changes; the *owner* of the content moves from template to agent.

- The controller embeds the resolved `AgentSpec` in `cmd.assign` (as today). The runner's
  `prepare()` writes `files` for **this run, in this run's worktree or the agent's home**, with the
  existing `merge: json` support for shared state files such as `~/.claude.json`.
- Hook configs call `wrangl runner hook <Event>` and MCP configs call `wrangl runner mcp`, which
  talk to the runner daemon's socket and are routed by run id. With several sessions on one
  runner this is already per run; an agent definition does not change it.
- The per-agent tables in `docs/wrangl/agents.md` (hook file path, event names,
  `--continue-format`, MCP path, trust flag) become the *content of the agent definitions*, shipped
  as seed definitions (`docs/wrangl/agents/*.yaml`) the owner imports with `wrangl agent create -f`.
  Seeds replace the agent half of today's template YAMLs; the template half keeps the setup.
- **Files global to the home directory are shared by concurrent sessions of different agents.**
  Codex's `~/.codex/hooks.json` or Gemini's `~/.gemini/settings.json` hold one set of hooks for the
  runner, not per worktree. Because `wrangl runner hook` routes by run id (from the environment
  wrangl sets for the agent process), a shared file with identical content is safe; two agent
  definitions that write *different* content to the same path on one runner are not. Rule: a
  runner may host sessions of several agents, but `prepare()` refuses (clear message, run goes
  back to the queue) if the run's merged-file content for a path differs from what a live
  session already wrote there. Agents with per-worktree config (Claude's `.claude/settings.local.json`,
  `.mcp.json`) have no such limit. This is the main reason for keeping the allow-list narrow on
  shared hosts.
- Secrets: `secrets` and `secret_files` belong to the agent. They are resolved in `assignOne`
  exactly as now (env travels over NATS TLS; secret files are written 0600 and removed when the
  run ends; the shared-path refcount already handles several sessions). The setup-token collection
  task (riAeMu) becomes "collect the secrets an agent names", and the "missing secret" error names
  the agent rather than the template.

### 5. Template versioning, self-upgrade and agents

Two separate things currently move runners to new versions: the **template version** (mise config
and setup script, applied on heartbeat) and the **wrangl binary** (`reconcileBinary`, only when the
runner is idle, which with sessions means zero sessions).

- **Agent edits do not bump the template version.** `agent.version` increments instead. A run uses
  whatever the agent's spec is when it is *assigned*; a session already running keeps the spec it
  started with (it was handed over in its assignment). No runner restart, no mise reinstall.
- **Allow-list edits do not bump the template version** either; they only gate future queueing.
  Removing an agent from a template while runs are queued with it makes those runs fail the
  assign-time check with the same clear message (they stay queued, visibly, like the "waiting for
  GitHub App" case, not failed).
- **Template edits that install or upgrade an agent binary** (`setup_script`, `mise_toml`) still
  bump the template version and are applied by the existing rollout. Order of operations for a
  change that needs both (a new agent flag that needs a newer CLI): update the template first;
  once runners report the new version, edit the agent. We do not couple them automatically in this
  ADR, but the template page shows which agents use it, and the agent page which templates allow it.
- **Compatibility with the `wrangl` binary.** Agent files reference `wrangl runner hook|mcp` and
  `--continue-format <fmt>`. A new agent definition may use a hook format or flag that an older
  runner binary lacks. The assignment therefore carries `agent.min_wrangl` (optional; the
  controller's own build is the default floor). A runner below the floor is treated like a runner
  marked for upgrade: not claimed for that run, and the idle-only upgrade picks it up. Without this
  field the first symptom would be a hook failing inside a live run.
- **Rollouts and the shared pool.** Since runners remain a shared pool across projects, a run is
  placed on any runner of its template with a free slot; the agent does not affect placement beyond
  the allow-list. Capacity caps stay per project x template (sX1mgv); they are not per agent.
- **Remote and special runners.** For a template whose machine is driven over SSH/ADB the agent
  runs on a *driver session* of the host runner (the PTY lives there). The allow-list on such a
  template names the agents that can drive it; the agent's `files` apply to the driver's worktree
  and home, not to the remote device.

## Consequences

Good:

- Choosing an agent no longer chooses a machine; project default and `--agent` work as the owner asked.
- One place per agent for commands, hooks, MCP files and secrets; seeds replace copy-paste.
- Agent edits stop triggering machine rollouts.
- Refusals are early and explicit.

Costs and risks:

- A migration with a one-release compatibility shim (`agent_json` fallback) and a naming
  heuristic the owner should review on first run (the migration logs what it created).
- The allow-list can drift from reality (the box lacks the binary). Mitigation: show agents per
  template; later, have template apply verify `command -v <start[0]>` and mark the agent
  unavailable on that template. Not in scope here.
- Shared home-directory config files make mixed-agent runners subtle (section 4); the refusal
  rule is simple but needs a clear message and a test.
- Two sorts of "agent" in the product (these, and the controller's conversational assistants).
  Rename the latter in the UI.
- `agent.min_wrangl` adds a field and a claim-time check.

## Out of scope / follow-ups

1. Per-task agent override (the resolution order leaves a slot for it).
2. Verifying the agent binary exists when a template is applied.
3. Dropping `runner_template.agent_json`.
4. Repo-defined agents (option D), if a trust model is wanted.
5. Per-agent concurrency or quota limits (see "Quotas" in `docs/wrangl/agents.md`; subscription
   logins are metered per account, which an agent-level cap could express).

## Questions for the owner

1. Is `agent.min_wrangl` wanted now, or acceptable to defer until a hook-format mismatch bites?
2. Should the refusal for two agents writing conflicting shared files be a hard error (as written),
   or should such agents simply not be allowed together on one template?
3. Naming: agents named after the binary (`claude`, `codex`, `gemini`, `antigravity`), with
   `-interactive` for the interactive drive. OK?

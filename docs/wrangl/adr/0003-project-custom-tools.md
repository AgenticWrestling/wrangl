# ADR 0003: Per-project custom tools (MCP servers and HTTP tools) with credentials

- Status: **Accepted, not built.** Nothing here exists yet: the project page still shows a "Custom tools" placeholder. Design only.
- Task: AJzSQY. Parts 2 (project page UI) and 3 (injection into runs) follow once this is accepted.
- Builds on: the controller secret store (`<data>/secrets`, write-only over the API; `wrangl secret set`), agent definitions and their hook/MCP files ([ADR 0001](0001-agent-definitions-vs-templates.md)), the work-stack MCP shim (`wrangl runner mcp`), run scopes (`runs.Scope`: kind, project, root)
- Touches (future work): `project` data model, `runs.ExecTool`, the runner's agent socket (`GET /tools`, tool relay), the assignment (`proto.Assignment`), `web/templates/project.html` ("Custom tools" placeholder), Setup credentials status

## Context

A project's agents sometimes need more than the repo and the work-stack tools: an issue tracker, a
deploy API, a database console, a documentation search. Today the owner can only edit the agent
definition (global to every project using it) or bake files into a template. The project page already
has a disabled "Custom tools" card ("Register tools for this project's agents (MCP servers, HTTP APIs,
CLIs) and store the credentials they authenticate with").

What exists and constrains the design:

- Agents reach wrangl through `wrangl runner mcp`, a stdio MCP server that every agent definition
  already registers (`.mcp.json`, `~/.codex/config.toml`, ...). The daemon answers `GET /tools` with
  `stacktools.For(kind)` and relays each `tools/call` to the controller (`ToolFor` -> NATS ->
  `runs.ExecTool`), which enforces scope (`Allowed(kind)`, run id, project). A new tool needs no new
  agent configuration if it is served through this shim.
- Secrets are files in the controller's secret store, referenced by name. Agent secrets are resolved
  at assignment and travel to the runner in `cmd.assign` (env, or 0600 files removed at run end).
  Values are never returned by the API. The agent process can read its own environment and files.
- Per-agent MCP config formats differ (JSON `.mcp.json`, TOML `config.toml`, `settings.json`), and
  home-directory config is shared by concurrent sessions on a runner (ADR 0001, section 4).
- Review and plan runs are read-only and must not gain write-capable tools by accident.

The questions: what is a "tool", where do its credentials live and who can read them, how does a tool
reach an agent of any kind, and how do we stop a registered tool becoming a hole (SSRF, secret
exfiltration, a tool that mutates things in a review run).

## Options

### Q1. How does a registered tool reach the agent?

**A. Write it into each agent's MCP config** (new `tools` rendering rules per agent definition: JSON
merge into `.mcp.json`, TOML for Codex, ...).
- Pros: agents talk to the real MCP server directly; works for stdio servers, which are the common
  packaging for community MCP servers.
- Cons: every agent needs a renderer and the formats drift; credentials land in config files or
  process environments on the runner, readable by the agent user and, for home-directory files, by
  other sessions; a remote HTTP tool would need the agent to hold its token.

**B. Proxy through the work-stack shim (recommended for HTTP and remote-MCP tools).** The project's
tools are served by `wrangl runner mcp` like `task_get`: the daemon lists them, the call goes to the
controller, the controller resolves credentials and calls the upstream, and returns the result.
- Pros: nothing to render per agent; works for every agent that already has the shim; credentials
  never leave the controller; one choke point for scope, rate limits, logging and SSRF rules.
- Cons: the controller makes outbound calls on the agent's behalf (needs egress controls); cannot
  serve a stdio server that must run next to the repo; one more hop of latency.

**C. A per-project tool gateway as a separate MCP endpoint** with a per-run token in each agent's
config (like the controller's own `/mcp`).
- Pros: standard MCP over HTTP; agents could use it without the shim.
- Cons: needs a per-agent config entry carrying a bearer token the agent can read; duplicates the
  shim's scoping; no benefit over B for agents that already have the shim.

### Q2. Where do credentials live and who can read them?

**A. In the existing secret store, referenced by name from the tool definition.** The definition holds
`{{secret.NAME}}` placeholders, never values. Values are written through the write-only API and CLI.
**B. A new encrypted per-project store in the database.**
- Pros: per-project isolation, rotation history.
- Cons: a second secret store with its own key management (no key material is managed today); the
  secret files are already 0600 and backed up with the data dir. Not worth it before the first tool.
**C. The tool carries inline values** (Rejected: they would appear in API responses, exports and logs).

### Q3. What can a tool be?

**A. HTTP tool**: a request template (method, URL, headers, JSON body) with a declared input schema.
**B. Remote MCP server** (Streamable HTTP URL plus headers) that the controller connects to.
**C. Local stdio MCP server** (a command run on the runner).
**D. Allow only whole-agent-config edits** (status quo).

## Decision

**Hybrid: Q1-B for HTTP tools and remote MCP servers, Q1-A (narrowly) for stdio servers; Q2-A;
phased delivery.**

### 1. Data model

```
project_tool (id, project_id -> project ON DELETE CASCADE, name, kind, spec_json,
              enabled, run_kinds, created_at, updated_at, UNIQUE (project_id, name))
```
- `name`: `[a-z0-9_]{1,40}`; exposed to agents as `ext_<name>` (a prefix no built-in tool uses, so
  a project tool can never shadow `task_complete` or another stack tool).
- `kind`: `http` | `mcp_remote` | `mcp_stdio`.
- `spec_json` per kind:
  - `http`: `{description, method, url, headers{K: "{{secret.KEY}}" | literal}, body_template?,
    input_schema, timeout_s?, max_response_kb?}`.
  - `mcp_remote`: `{url, headers{...}, allow_tools? [names]}`. The controller lists the server's tools
    at registration and on a refresh button, and exposes the allowed ones as `ext_<name>_<tool>`.
  - `mcp_stdio`: `{command, args, env{K: "{{secret.KEY}}"|literal}, setup?}` (see section 4).
- `run_kinds`: which run kinds see the tool; default `work,revise`. Review and plan runs get none
  unless the owner ticks them, and tools marked `read_only` (a registration-time claim by the owner,
  not verified) are the only ones offered by default to them.
- Secret names: a tool's placeholders `{{secret.KEY}}` resolve to the secret named
  `tool.<project>.<tool>.<key>`. They are set with `PUT /api/v1/secrets/{name}` (already write-only)
  or `PUT /api/v1/projects/{slug}/tools/{tool}/secrets/{key}` (a convenience that builds the name).
  Names stay `[a-z0-9_.-]+`, which the store already requires. Deleting a tool does not delete its
  secrets silently: the UI offers it, and `wrangl secret list` shows them.

### 2. Reaching the agent (phases 1 and 2: proxied tools)

- At assignment the controller computes the run's tool list (project, run kind, enabled) and includes
  it in `cmd.assign` as declarations only: name, description, JSON input schema. **No credentials.**
- The daemon's `GET /tools` appends those declarations to `stacktools.For(kind)`; `tools/call` for an
  `ext_` name is relayed to the controller like any stack tool.
- `runs.ExecTool` resolves `ext_` names from the run's project (taken from the run, never from
  arguments), enforces `run_kinds`, resolves secrets, performs the call, and returns the body
  (truncated to `max_response_kb`, default 64) or a clear error.
- The controller **never** includes secret values in tool results it logs; headers and bodies are
  redacted in the audit log by the placeholder values.

### 3. Safety rules for the controller's outbound calls

- `https` only (`http` only for loopback in tests); URLs are fixed at registration apart from
  declared path/query placeholders filled from arguments after URL-encoding; the **host cannot come
  from tool arguments**.
- Deny private, loopback, link-local and metadata ranges after DNS resolution (and re-check on
  redirects, at most 3, same host only) unless the owner sets `allow_private: true` on that tool
  with a warning in the UI.
- Per-call timeout (default 30 s, max 120), response cap, per-tool and per-run rate limit.
- One audit row per call (run, tool, status, duration, bytes), no bodies or headers.
- The tool test button in the UI calls with owner-supplied arguments and shows the response, so the
  owner learns what the agent will see.

### 4. stdio MCP servers (phase 3)

A stdio server must run on the runner, next to the checkout. It is the only kind that places a
credential on the runner:

- The agent's MCP config gets one entry per stdio tool, rendered by a **new optional section of the
  agent definition** (`tools.render`: the file path and the JSON/TOML fragment for one server). Agents
  without it cannot use stdio tools, and the project page says so.
- The entry does not contain the secret. It runs `wrangl runner tool-run <name>`; the daemon, which
  holds the assignment, starts the real command with the resolved environment. The secret is never in
  a file, but **the server process runs as the agent user, so the agent can read its environment**.
  This is inherent; the UI says so next to the option ("this tool's credentials are visible to the
  agent"). Use a proxied tool where the credential is sensitive.
- Config files are per run where the agent allows it (Claude's `.mcp.json` is in the worktree). For
  home-directory formats, ADR 0001's rule applies: refuse a run whose rendered content conflicts with
  a live session's on the same runner.
- Installing the server's package is the template's job (`mise_toml` / `setup_script`), as for agent
  binaries. `setup` in the spec is documentation only.

### 5. Project page and CLI (part 2)

The "Custom tools" card lists tools with kind, enabled toggle, run kinds, and per-secret `✓ set` /
`! missing` (the same pattern as Setup > Agent credentials). Add/edit is a sheet with a form per kind
and a Test button. CLI: `wrangl tool list|show|add -f spec.yaml|edit|rm|test` per project and
`wrangl tool secret set PROJECT TOOL KEY` (stdin). API under `/api/v1/projects/{slug}/tools`.
Editing a tool applies to runs assigned afterwards; running sessions keep what they were given.

### 6. Failure modes

| Failure | Behaviour |
|---|---|
| Secret missing at call time | The call returns a clear error naming the secret; the run is not failed. The project page shows `! missing`. Optionally (follow-up) a missing secret on an `enabled` tool holds a run, like a missing agent secret. |
| Upstream slow or 5xx | Error with status, no retry by default (tools may not be idempotent). |
| Upstream response huge or hostile | Truncated; returned as data inside a tool result. Prompt injection through tool output is possible and is not solved here; see consequences. |
| Tool removed or disabled mid-run | Calls return "tool not available"; the run continues. |
| Redirect to a private host / DNS rebinding | Blocked by the post-resolution check and the same-host redirect rule. |
| Controller restart | Tool definitions are in the database; nothing is held in memory. |

## Consequences

Good:
- Registering an HTTP or remote-MCP tool needs no per-agent work and exposes no credential to the
  runner or the agent.
- One enforcement point for scope, SSRF rules, limits and audit.
- Names are namespaced; read-only runs do not gain tools by default.

Costs and risks:
- The controller becomes an HTTP client for owner-registered URLs: the egress rules above are
  essential, and a controller in a VPC can reach internal services unless `allow_private` is off.
- A remote-MCP proxy needs an MCP client in the controller (sessions, `tools/list`, notifications);
  streaming and long-running tools are limited by the request/response relay and its 30 s NATS
  timeout today (`ToolFor` uses a 30 s context). Long calls need either a larger bound or an
  async pattern.
- Stdio tools put credentials in a process the agent can inspect; they also depend on per-agent
  renderers. They are the last phase for a reason.
- Tool output is untrusted input to the agent: a tool that returns attacker-controlled text can
  steer it. The owner chooses the tools; wrangl labels tool results but cannot sanitise them.
- A second naming convention for secrets (`tool.<project>.<tool>.<key>`) lives in the global store.

## Phasing

1. `http` tools: table, API/CLI, secrets, project card, proxy through the shim, audit, SSRF rules.
2. `mcp_remote`: MCP client in the controller, tool discovery and the allow list.
3. `mcp_stdio`: `tools.render` in agent definitions, `wrangl runner tool-run`, conflict rule.

## Questions for the owner

1. Is the hybrid right, or should stdio servers be out of scope (phases 1-2 only)?
2. Should project tools be allowed in review/plan runs at all, or never (simpler, safer)?
3. Is `allow_private` (internal services reachable from the controller) wanted, or should private
   addresses stay forbidden everywhere?
4. Should a missing tool secret hold the run (like a missing agent secret) or only fail the call?
5. Do you want per-tool approval for calls with side effects (an `ask_question`-style confirmation),
   as a later phase?

# ADR 0003: Custom tools: controller-executed, defined in KDL, a system-wide library

- Status: **Proposed** (the owner's decisions below are settled; this document awaits the owner's sign-off on the plan before phase 1 starts)
- Task: 4VL3d2 (plan: Q1rp6C). Supersedes the earlier per-project draft of this ADR (task AJzSQY).
- Builds on: the controller secret store (`<data>/secrets`, write-only over the API; `wrangl secret set`), the work-stack MCP shim (`wrangl runner mcp`), run scopes (`runs.Scope`: kind, project, root), approval questions (`ask_question`), [ADR 0001](0001-agent-definitions-vs-templates.md)
- Touches (future work): a `tool` library table and per-project enablement, `runs.ExecTool`, the runner's agent socket (`GET /tools`, tool relay), `proto.Assignment` (declarations only), Settings > Tools, the project page ("Custom tools" card), the run page (audit), Setup credentials status, `SKILL.md`, `INSTALL.md`

## Context

A project's agents sometimes need more than the repo and the work-stack tools: a deploy API, an
issue tracker, `kubectl`, a documentation search. Today the owner can only edit the agent definition
(global to every project using it) or bake files into a template.

What exists and constrains the design:

- Agents reach wrangl through `wrangl runner mcp`, a stdio MCP server that every agent definition
  already registers. The daemon answers `GET /tools` with `stacktools.For(kind)` and relays each
  `tools/call` to the controller (`ToolFor` -> NATS -> `runs.ExecTool`, `cmd/wrangl/internal/controller/runs/tools.go`),
  which enforces scope (`stacktools.Allowed(kind)`, run id, project). A tool served through this shim
  needs no new agent configuration.
- Secrets are files in the controller's secret store, referenced by name and never returned by the
  API. Agent secrets are resolved at assignment and travel to the runner in `cmd.assign`.
- **An agent can read the environment and files of anything that runs as its user** (`/proc/<pid>/environ`,
  its own env, files it can open). Anything placed on a runner is therefore visible to the agent.
- Per-agent MCP config formats differ and home-directory config is shared between concurrent
  sessions on a runner (ADR 0001, section 4). Serving tools through the shim avoids all of it.
- Relayed calls are bounded by a 30 s context (`ToolFor`, NATS request/reply).
- Review and plan runs are read-only and must not gain write-capable tools by accident.
- The runner pushes the run branch at every checkpoint (commit d452f51), so the controller can fetch
  the run's committed work.

## Decisions (settled by the owner)

1. **The controller host is the primary executor.** A runner sees only tool *declarations*: name,
   description, input schema, run kinds. Credentials, argv and URLs stay on the controller and are
   never shipped to runners. Calls use the existing path: agent -> MCP shim -> daemon -> NATS ->
   `runs.ExecTool`, where scope (run, kind, project) is enforced. The project comes from the run,
   never from arguments.
2. **Executor kinds, all on the controller:**
   - `http`: the controller makes the request.
   - `mcp_remote`: the controller is the MCP client of a remote (Streamable HTTP) server.
   - `exec`: a CLI run in a sandbox on the controller.
   - `mcp_stdio`: a stdio MCP server hosted (sandboxed) on the controller.

   A **runner-side executor** is an explicit, per-tool opt-in escape hatch for things that must run
   next to the checkout. It never carries secrets, and the UI states why: the process runs as the
   agent's user, and an agent can read the environment of anything that runs as its user. It is not
   in the phase plan below; it needs its own ADR before it is built.
3. **The sandbox is mandatory before `exec` and `mcp_stdio` exist** (phase 2 ships it first). See
   "Sandbox".
4. **Arguments are untrusted.** The command is argv-only, never a shell. `{{input.x}}` fills a whole
   argv element only, never part of one. Inputs are validated by the declared schema (enum, pattern,
   integer, length). A value starting with `-` is rejected unless the schema allows it. The binary
   and fixed args belong to the owner.
5. **Repo-dependent tools take an input ref** (the run branch). The controller checks the pushed commit
   out into scratch space. Uncommitted work is not visible, and the tool help says so.
6. **Results:** size cap (default 64 KB), exact-value scrubbing of the tool's secrets from output, and
   an async pattern for calls over the 30 s NATS bound (accept, progress, result later).
7. **Effect:** each tool declares `read | write | destructive`. Destructive asks the owner first
   (reusing the approval question). Review and plan runs only get `read` tools. One audit row per call
   (run, tool, status, duration, bytes), shown on the run page.
8. **A system-wide library** (Settings > Tools), **enabled per project** with **per-project secrets**.
   Projects do not redefine tools. The project page shows enabled tools with `set` / `missing` per
   secret, like Setup > Agent credentials.
9. **Definition syntax is KDL v2**, parsed with `github.com/njreid/gokdl2` behind one small package.

## Definition syntax (KDL v2)

```kdl
tool "deploy_status" {
  description "Show the deployment status of an environment"
  effect "read"
  run-kinds "work" "revise"
  input {
    param "env" type="string" { enum "staging" "prod" }
  }
  exec {
    argv "kubectl" "get" "deploy" "-n" "{{input.env}}" "-o" "json"
    secret "KUBECONFIG" name="kube.prod" as="file"
    network "full"
    timeout 60
  }
}
```

- `http` tools use `http { method; url; header; body }`; `mcp_remote` tools use
  `mcp-remote { url; header; allow-tool }`. Exactly one executor node per tool.
- Secrets are referenced **by name only**, never inline (`secret "ENV_VAR" name="store.name" as="env"|"file"`;
  `header "Authorization" "Bearer {{secret.api_token}}"` for http). A definition containing something
  that looks like an inline credential is rejected at validation where detectable.
- `name`: `[a-z0-9_]{1,40}`, exposed to agents as `ext_<name>` so it can never shadow a stack tool.
  `run-kinds` defaults to `work revise`; review/plan are added only for `effect "read"` tools.
- Secret store names are `tool.<tool>.<key>` for the library default placeholder and
  `tool.<project>.<tool>.<key>` for the per-project value; a project's value is what is used.
  Names stay `[a-z0-9_.-]+`, which the store requires.

### KDL parser: `github.com/njreid/gokdl2` (researched)

- **Licence:** MIT, so compatible with our use. It is a fork of `sblinch/kdl-go`.
- **API (from the README):** package `kdl`; `kdl.Parse(reader)` parses with automatic version
  selection (v2 first, then v1); `ParseVersionV2` / `ParseVersionV1` / `ParseVersionAuto` pick a
  version explicitly; the result is a document with `Version` and `Nodes`, with node names read via
  `NodeNameStringV2()`; also struct/map marshal and unmarshal, custom marshaler interfaces and
  document generation. Exact signatures were not visible in the README and must be confirmed against
  the source before phase 1.
- **Maturity:** the README shows no releases, but the module has a tag (v0.6.1, pinned in phase 1a), 46 commits, 16 stars, one open issue. Last-commit date
  was not visible. It vendors the official KDL v1 and v2 compliance suites.
- **Consequences:** pin the tag in `go.mod`; always call `ParseVersionV2` (never auto, so a v1 file is
  a clear error, not a silent reinterpretation); decode into our own structs from the node tree rather
  than relying on reflection-based unmarshal; wrap everything in one package (`internal/toolkdl`:
  `Parse([]byte) (Definition, error)` and `Format(Definition) []byte`) so the parser can be swapped.
  Fuzz and golden-file tests live in that package. Check `go mod vendor`/licence listing at phase 1.

## Architecture

### Data model

```
tool            (id, name UNIQUE, kind, effect, description, spec_kdl, spec_json,
                 run_kinds, created_at, updated_at)            -- the system-wide library
project_tool    (project_id -> project ON DELETE CASCADE, tool_id -> tool, enabled,
                 PRIMARY KEY (project_id, tool_id))
tool_audit      (id, run_id, project_id, tool, status, duration_ms, bytes, at)
```

The KDL text is the source of truth (`spec_kdl`, for `wrangl tool show`/`edit`); `spec_json` is the
validated form the executor uses. Editing a tool applies to runs assigned afterwards; running sessions
keep the declarations they were given, but the controller always executes the current definition and
re-checks that the tool is still enabled (a removed tool returns "tool not available", the run continues).

### Call path

1. At assignment the controller computes the run's list (project's enabled tools, filtered by run
   kind and effect) and puts **declarations only** in `cmd.assign`.
2. The daemon's `GET /tools` appends them to `stacktools.For(kind)`; `tools/call` for an `ext_` name is
   relayed like any stack tool.
3. `runs.ExecTool` resolves the tool from the run's project, enforces `run_kinds` and effect, validates
   the input against the declared schema, asks for approval if destructive, resolves secrets, runs the
   executor, scrubs and caps the output, writes the audit row and returns the result.

### Effect and approval

`read` tools are offered to all their `run-kinds`; `write` to work/revise only; `destructive` to
work/revise and each call first asks the owner through the existing approval question
(`SystemQuestionTool` path) naming the tool and its arguments; no answer, no call. The effect is the
owner's claim and is not verified; the library UI says so.

### Results, scrubbing, async

- Output is capped (default 64 KB, per-tool override with a hard maximum) and marked truncated.
- Every resolved secret value of the call is removed from output and error text by exact match
  (also its base64 and URL-encoded forms) before the result is logged, audited or returned.
  Values never appear in logs, audit rows or assignments (tested).
- Calls over the 30 s bound use accept / progress / result: `ExecTool` returns a call id at once
  (`status: running`), the tool is then polled through an `ext_result` companion tool with that id, and
  progress lines are retained for the poll. Tools declare `timeout` (default 30 s, max 600 s); those
  above 25 s are always async. Results are held in memory with a TTL and in the audit row's status.
  Details are fixed in the phase 1 PR; the 30 s relay bound itself is not raised.

### http tools: SSRF rules

`https` only (`http` to loopback only in tests); the URL is fixed by the owner apart from declared
path/query placeholders filled after URL-encoding; the **host never comes from arguments**. Private,
loopback, link-local and metadata ranges are denied **after DNS resolution** (the dialer connects to
the checked IP, which defeats rebinding), at most 3 redirects, same host only. Timeout default 30 s
and a per-tool and per-run rate limit. A per-tool `allow-private` flag is not in this plan (private
addresses stay forbidden); it can be added later by the owner's decision.

### repo-dependent tools

A tool with a `ref` input (the run's branch, defaulted by the controller) gets the pushed commit
checked out by the controller into its scratch directory (`git fetch` of the run branch from the
project's remote, `git worktree`/archive at the resolved sha, removed after the call). Commit
d452f51 makes the runner push the branch at each checkpoint; **uncommitted work is not visible** and
the tool's help text says so. A ref that does not resolve to a pushed commit is an error naming it.

### Sandbox (before `exec` and `mcp_stdio`)

Every tool process runs:

- as a separate unprivileged user (never the wrangl service user), in its own process group;
- under bubblewrap or `systemd-run` (see the host check below) with a read-only root and a private
  scratch dir that is the working directory and is removed afterwards;
- with **no access to the wrangl data dir or secret store** (not mounted, and the data dir is also 0700
  to that user);
- with a minimal environment containing only the declared secrets (as env var, or as a 0600 temp file in
  scratch for the call only);
- with network **none** by default, enabled per tool by `network "full"` (anything else needs a new
  mode in the spec; there is no per-host filtering in this plan);
- with CPU, memory, PID and wall-clock limits (the declared `timeout`, hard-capped), the whole process
  group killed on timeout, cancel or controller shutdown;
- under a global cap on concurrent tool processes (default 4, configurable; excess calls queue briefly then
  fail with a clear error) so tools cannot starve the scheduler and web UI.

`mcp_stdio` servers use the same sandbox and are long-lived per run (started on first call, stopped at run end
or after idle), subject to the same cap.

**Host check (this build host, Amazon Linux 2023, aarch64, kernel 6.1, systemd 252):**

- `bubblewrap` 0.12.0 is available from the Amazon Linux repo (`dnf install bubblewrap`) and is not installed by default.
  Unprivileged user namespaces are enabled (`user.max_user_namespaces` = 30614), so `bwrap` works without root.
  It provides read-only bind mounts, `--unshare-net`, `--unshare-pid`, `--die-with-parent`, `--clearenv` and tmpfs scratch
  directly; it has no cgroup limits, so CPU/memory/wall-clock come from `ulimit`/`prlimit` and our own timeout.
- `systemd-run` 252 is present, but with no user session bus in this environment (`systemd-run --user` fails with
  "Failed to connect to bus"). A system-scope `systemd-run` needs root or polkit, and the wrangl service should not
  be root. It offers real cgroup limits (`MemoryMax`, `CPUQuota`, `TasksMax`) and `DynamicUser=`, but only if the controller
  may call it.
- **Recommendation:** bubblewrap as the primary primitive, with limits via `prlimit` and a timeout/process-group kill
  in our code; use `systemd-run` as an optional wrapper for cgroup limits where the service is allowed to (a unit file
  running the controller with `Delegate=yes` is the clean way). The sandbox code sits behind a small interface
  (`sandbox.Run(spec)`), and `wrangl doctor`/startup checks refuse to enable `exec`/`mcp_stdio` if `bwrap` is missing or
  fails a self-test (it runs `bwrap ... true`, and checks the data dir is not visible). INSTALL.md gets the
  `dnf install bubblewrap` step. This must be re-verified on the actual controller host before phase 2; the check above
  was done on the machine this plan was written on.

### CLI, API, UI

- CLI: `wrangl tool list|show|add -f tool.kdl|edit|rm|enable|disable|test` and `wrangl tool secret set` (value
  from stdin, never an argument). `enable`/`disable` take `--project`. `test` runs a call with owner-given
  arguments as the owner, shows the exact result the agent would see, and writes an audit row without a run.
- API: `/api/v1/tools` (library), `/api/v1/projects/{slug}/tools` (enablement), secret set under the project
  tool. Secrets are write-only.
- UI: Settings > Tools (list, KDL editor with validation, Test); the project "Custom tools" card (enable toggle, run
  kinds, `set` / `missing` per secret); the run page shows the audit rows.

### Failure modes

| Failure | Behaviour |
|---|---|
| Secret missing at call time | The call errors naming the secret; the run is not failed. The project page shows `missing`. |
| Upstream slow or 5xx | Error with status, no retry (tools may not be idempotent). |
| Output huge or hostile | Truncated and scrubbed; returned as data. Prompt injection through tool output is not solved. |
| Tool removed or disabled mid-run | "tool not available"; the run continues. |
| Redirect to a private host / DNS rebinding | Blocked by the post-resolution check and the same-host rule. |
| Sandbox unavailable | exec/mcp_stdio refuse to enable; calls fail closed, never run unsandboxed. |
| Controller restart | Definitions are in the DB; in-flight async calls are marked failed (`controller restarted`). |

## Consequences

Good: no per-agent rendering, no credential on any runner, one enforcement point for scope, SSRF, limits,
approval and audit; names are namespaced; review/plan runs only get read tools.

Costs and risks:
- The controller becomes an outbound HTTP client and a process host for owner-defined commands. The SSRF
  rules and the sandbox are essential, and the sandbox is the largest piece of work.
- Controller capacity is shared with tool processes (hence the concurrency cap).
- `mcp_remote` needs an MCP client (sessions, `tools/list`, notifications) in the controller.
- Tool output is untrusted input to the agent; wrangl labels results but cannot sanitise them.
- The KDL library is young and unreleased; mitigated by pinning and the wrapper package.
- Tools that must run in the checkout cannot see uncommitted work; the ref model trades that for safety.

## Phase plan and PRs (one PR each, with tests)

1. **Library, KDL, CLI, API, UI, http proxy, audit, SSRF.** PRs, in order:
   1a. `internal/toolkdl`: gokdl2 wrapper, definition structs, validation, golden/fuzz tests.
   1b. DB migration + queries (`tool`, `project_tool`, `tool_audit`) and the service layer with scope/effect rules.
   1c. API + CLI (`wrangl tool ...`, `tool secret set`) and secret naming.
   1d. Declarations in `cmd.assign` / `GET /tools`; `ExecTool` dispatch, schema validation, audit, scrubbing, size cap.
   1e. `http` executor with SSRF rules (dialer, redirects) and the async pattern.
   1f. Web: Settings > Tools, project enable card with secret status, audit on the run page.
   1g. Approval for destructive tools; SKILL.md and INSTALL.md.
   Tests: e2e with a fake upstream; a test that no secret value reaches a runner assignment, a log or an audit row;
   review/plan runs see only read tools.
2. **Sandbox + `exec` kind.** Sandbox package (bwrap, limits, process-group kill, concurrency cap, self-test), exec
   executor, repo `ref` checkout. Tests include a **hostile-argument test** (leading `-`, spaces, quotes, `;`, `$()`,
   newlines, path traversal, oversized values: none reaches a shell or alters argv structure) and sandbox escape checks
   (data dir and secret store invisible, no network by default, fork bomb and memory limits).
3. **`mcp_remote`.** MCP client, discovery, `allow-tool`, namespacing, SSRF rules reused. e2e with a fake MCP server.
4. **`mcp_stdio`.** Controller-hosted sandboxed server lifecycle, reuse of sandbox and cap. e2e with a fake stdio server.

SKILL.md, INSTALL.md and this ADR's status are updated in the PR that completes each phase, and the status moves to
Accepted when phase 1 merges.

## Open points (not blocking sign-off)

- A runner-side executor opt-in needs its own ADR.
- A missing tool secret fails the call only; holding runs on it (like agent secrets) is a possible follow-up.
- `allow-private` for internal services is deliberately absent; revisit on request.

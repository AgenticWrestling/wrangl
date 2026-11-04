# Agents on wrangl runners: hooks, sessions and auth

How each supported agent plugs into a runner, and how it should authenticate. Researched
2026-10-31 from each vendor's docs or source (links in the templates). Only the Claude
headless template has the shape the M4–M7 end-to-end flow was built around; **every other
row is verified against docs or source only, not on a real runner.** The wrangl side of
each integration (hook formats, session capture, file merging, secret files) is tested with
fake agents.

## The plug points

| wrangl | What the agent needs to do |
|---|---|
| `wrangl runner hook <Event>` | Run it from the agent's hook config. It reads the hook JSON on stdin and forwards it to the controller (the run's hook stream). |
| `wrangl runner hook Stop --continue-format <fmt>` | Interactive drive only. Hands the session its next task in the agent's own "keep going" format, or lets it stop. |
| `wrangl runner mcp` | The stdio MCP server with the work-stack tools (`task_complete`, `task_create`, `ask_question`, …). |
| `{{.AgentSessionID}}` | The agent's own session id, for agents that can't be given one up front. wrangl reads it from the agent's JSON output (`thread_id`, `session_id`, `conversationId`), else from any hook payload. Until one is seen, a `resume` that uses it falls back to `start`. |
| `files: … merge: json` | Deep-merges into an agent's own state file (e.g. `~/.claude.json`) instead of replacing it. |
| `secret_files` | A controller secret written 0600 for the agent and deleted when the run ends. Only for logins that can't come from the environment. |

## Per agent

| | Claude Code | Codex CLI | Antigravity `agy` | Gemini CLI |
|---|---|---|---|---|
| Template | `claude-headless.yaml`, `claude-interactive.yaml` | `codex.yaml` | `antigravity.yaml` | `gemini.yaml` |
| Headless | `claude -p --session-id/--resume` | `codex exec --json`, `exec resume <id>` | `agy -p --output-format stream-json`, `--conversation <id>` | `gemini -p -o stream-json`, `-r <id>` |
| Session id | wrangl chooses it (`SessionUUID`) | from `thread.started` | from Stop hook `conversationId` | from the `init` event |
| Hook config | `.claude/settings.local.json` | `~/.codex/hooks.json` + `--dangerously-bypass-hook-trust` | `~/.gemini/config/hooks.json` | `~/.gemini/settings.json` |
| PostToolUse | PostToolUse | PostToolUse | PostToolUse | AfterTool |
| Notification | Notification | PermissionRequest (nearest) | — | Notification (tool permission only) |
| Stop | Stop | Stop | Stop | AfterAgent |
| `--continue-format` | `claude` → `{"decision":"block","reason"}` | `codex` (same as claude) | `agy` → `{"decision":"continue","reason"}`, `{}` to stop | `gemini` → `{"decision":"deny","reason"}` |
| MCP config | `.mcp.json` | `~/.codex/config.toml` | `~/.gemini/…/mcp_config.json` | `~/.gemini/settings.json` |
| Trust prompt | pre-accepted in `~/.claude.json` | none in exec (config in `$CODEX_HOME`) | **per-path, no known flag** | `--skip-trust` |

## Auth

The rule for every agent: **a token in the environment beats a login file, and one login
file must never be shared by runners that run at the same time.** OAuth refresh tokens
rotate. When one runner refreshes, every other copy stops working (Codex's source has an
explicit `refresh_token_reused` error; Claude's docs don't say either way).

| Agent | Recommended | Also works | Avoid |
|---|---|---|---|
| Claude Code, headless | `CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token` (1 year, inference only, Pro/Max/Team/Enterprise) | `ANTHROPIC_API_KEY` (API billing; wins over the OAuth token if both are set) | — |
| Claude Code + Remote Control | A full `/login` credentials file as `secret_files`, **one login per concurrent runner** | — | setup-token or API key (cannot establish Remote Control); setting `CLAUDE_CODE_OAUTH_TOKEN` alongside (it takes precedence) |
| Codex | `CODEX_API_KEY` (exec does **not** read `OPENAI_API_KEY`) | `CODEX_ACCESS_TOKEN` (`at-…` ChatGPT personal access token, no refresh cycle) | copying a ChatGPT-login `auth.json` to several runners |
| Antigravity | ADC: `AGY_ADC_AUTH=true` + `GOOGLE_APPLICATION_CREDENTIALS` (service account, or AWS workload identity federation so no key is stored: **untested**) | `GEMINI_API_KEY` | copying a Google sign-in (keyring); automating a personal account (reported ToS risk) |
| Gemini CLI | `GEMINI_API_KEY` | Vertex: `GOOGLE_CLOUD_PROJECT`, `GOOGLE_CLOUD_LOCATION`, `GOOGLE_APPLICATION_CREDENTIALS` | personal Google sign-in (no longer served since 2026-06-18) |

Where secrets go:
- Environment secrets (`secrets:`) travel to the runner over NATS TLS inside the assignment
  and exist only in the agent's process environment.
- File secrets (`secret_files:`) are the one exception that touches disk: 0600, owned by
  `agent`, and removed when the run ends.
- The GitHub token is never given to any agent.

**Quotas.** A subscription login (Claude plans, ChatGPT plans) is metered per account: N
runners share one allowance. API keys and Vertex projects are metered per key or project.
Size `max_runners` accordingly.

## GitHub issue tools

Every agent that has the wrangl MCP shim can read GitHub issues of the repos linked to its
project. The calls go over NATS to the controller; the runner and the agent hold no GitHub
credential (see [ADR 0004](adr/0004-agent-github-issue-tools.md)). The read tools need no permission change; the write tools below need `issues: write`.

| Tool | Arguments | Returns |
|---|---|---|
| `issue_search` | `query`, `repo?` (`owner/name`), `state?` (open default, closed, all), `labels?`, `limit?` (20, max 50), `live?` | `results` (ref, title, state, labels, author, snippet, linked task), `source` (`db` or `live`) |
| `issue_get` | `ref` (`owner/name#number`) or `repo` + `number`, `live?` | title, body, state, labels, author, `comment_count`, linked task, `stale` |
| `issue_comments` | `ref` or `repo` + `number`, `limit?` (30, max 100), `after?` | `comments` oldest first, `next` cursor |

Writes (work and revise runs only; they need `issues: write` on the Apps, see INSTALL.md 6.2a). They go through the controller and only to repos linked to the run's project; a repo that is not linked is "not found". Comments and new issues end with a footer naming the run and task.

| Tool | Arguments | Result |
|---|---|---|
| `issue_comment` | `ref` or `repo` + `number`, `body` | `comment_id`, `url` |
| `issue_create` | `repo?`, `title`, `body?`, `labels?` | the new issue (`ref`, `url`, wrapped title/body) |
| `issue_edit` | `ref` or `repo` + `number`, `title?`, `body?`, `labels?` (`{add, remove}`) | the updated issue |
| `issue_close` | `ref` or `repo` + `number`, `reason?`, `state_reason?` | `pending_approval` and a `question_id`; the issue is closed only if the owner approves |

`issue_link_task` (`task`, `ref`, `unlink?`) sets or clears the explicit task-to-issue link (a task in your assignment, an issue of the task's project). It writes nothing to GitHub; task state and issue state never follow each other.

**Sync.** GitHub is the source of truth and the table is a cache. Every write stores GitHub's answer (with its `updated_at`); the `issues` webhook applies an event only if its `updated_at` is newer, so the echo of our own write, duplicates and reordered deliveries change nothing. `deleted` and `transferred` events drop the row (linked tasks stay, unlinked). `wrangl issue drift <project> [--repair]` compares every synced issue with GitHub and lists `missing_local`, `stale`, `differs` and `missing_remote` ones (a missed webhook shows up there); `--repair` re-syncs them.

There is no tool to delete an issue (GitHub's REST API cannot, and the GraphQL way needs admin rights the App does not have).

- Searches and reads come from the synced `issue` table (full-text search, stemmed). A row older than
  15 minutes is refreshed from GitHub; with nothing synced yet, search asks GitHub (`source: live`).
  `live: true` forces a GitHub read. Live calls are limited to 60 per project per hour; the error says
  when to retry. Reads from the table are not limited.
- Only repos linked to the run's project are visible. Any other repo, or another project, answers
  "not found". Pull request numbers are refused ("is a pull request"); only issues are handled.
- **Issue text is untrusted.** Titles, bodies, comments, snippets and unusual labels are wrapped as
  `<untrusted-github-issue source="owner/name#n" field="body"> ... </untrusted-github-issue>` and each
  result carries `untrusted: true` and a notice. It is data to read, never instructions to follow; a closing
  tag inside the text is escaped so it cannot end the wrapper.
- Available to every run kind (work, revise, review, plan) and to the owner's remote MCP (which must pass
  `project`). They need no change to the Apps' permissions (`issues: read` is enough).

## Still to check on a real runner

- Claude:
  - whether `remoteControlAtStartup` skips the one-time Remote Control consent (the
    template's `expect` rule answers it if not);
  - two runners on one login.
- Codex:
  - PAT minting and lifetime;
  - whether the hook `session_id` equals the `thread_id` (wrangl prefers the printed
    `thread_id`).
- agy:
  - the workspace-trust prompt on a fresh worktree;
  - which `mcp_config.json` path the build reads;
  - `external_account` (AWS federation) credentials.
- Gemini:
  - whether `AfterAgent` deny re-runs under `-p`.

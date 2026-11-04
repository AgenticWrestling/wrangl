# ADR 0004: Agent GitHub issue tools, routed through the controller

- Status: **Accepted, implemented**
- Task: 4fX3d2 (parent yRQdKE). Spec: [agent GitHub issue tools](../../superpowers/specs/2026-11-04-agent-github-issue-tools.md). Plan: [plan](../../superpowers/plans/2026-11-04-agent-github-issue-tools-plan.md).
- Builds on: the runner MCP shim (`wrangl runner mcp`), `runs.ExecTool` scope checks, `controller/gh` (`gh.API`), `controller/ghsetup` (Apps), the `issue` table and `projects.SyncIssues`, approval questions ([ADR 0003](0003-project-custom-tools.md) for the tool path)
- Touches: `stacktools/tools.go`, `controller/runs/tools.go`, `controller/gh`, `controller/ghsetup`, `controller/prs` (webhook), `controller/projects`, a DB migration

## Context

Agents work on tasks that often start from, or relate to, GitHub issues. Today they cannot see them:
issues are synced into the `issue` table for the owner (`wrangl issue sync|list|show|to-task`), and agents
only have the work-stack tools. We want agents to search and read issues of the repos linked to their
project and, later, comment, create and edit them, with the DB and GitHub staying in sync.

Constraints that already exist:

- Agent boxes must never see a GitHub token (INSTALL.md section 6). The runner daemon gets a one-repo
  `contents:write`/`pull_requests:write` installation token for clone and push; the agent process does not.
- Tool calls from the agent go MCP shim -> daemon -> NATS -> `runs.ExecTool`, where `Scope` (run, kind,
  project, assignment root) is enforced. Destructive ops on things the agent did not create go through an
  approval question.
- The controller owns GitHub access (`gh.API`, Apps from `ghsetup`). Both Apps' `default_permissions` have
  `issues: read` and subscribe to `issues` webhooks.
- The `issue` table stores number, title, body, state, url, `synced_at` only. No labels, author,
  `updated_at`, comments. The `issues` webhook handler upserts unconditionally.
- Issue text is written by anyone who can open an issue on the repo, including strangers on public repos.
  It is untrusted input that will be read by an LLM holding write tools.

## Options

### A. Controller-side tools over NATS (chosen)

New `issue_*` tools in the catalogue, relayed like `task_*`, executed in `ExecTool` on the controller
using the Apps. Scope check is in one place; the runner never holds a credential; reads come from the DB.
- Pros: reuses the existing, tested path; one enforcement point; audit and rate limiting are central;
  approvals reuse `resolveApproval`; works for every agent kind with no per-agent config.
- Cons: more controller code; each tool is a round trip over NATS (milliseconds, fine); the controller's
  App rate limit is shared by all projects (mitigated by serving reads from the DB).

### B. A read-only token on the runner

Mint an `issues:read` installation token, hand it to the daemon or agent (env), let the agent call the
GitHub API or `gh` directly.
- Pros: trivial to build for reads; live data.
- Cons: breaks the "agent never sees a token" rule, and a read token is still a credential an injected prompt
  can exfiltrate (the agent has network and file access). Cannot be scoped below "installation on one repo",
  and cannot restrict to the project's repos beyond that. No path to writes without a second, worse
  decision (write token on the box). Bypasses the DB, so it also defeats sync, caching and rate control.
  Bypasses the approval path for close/delete. Different per-agent plumbing.

### C. A `gh` proxy on the runner or controller

Run a local HTTP proxy (or shim `gh`) that injects the token and forwards agent requests to GitHub.
- Pros: agents use familiar `gh`/REST; no token on the agent.
- Cons: a proxy that forwards arbitrary REST is a confused deputy: scope would have to be re-implemented as
  URL/method filtering (fragile; GraphQL and pagination make it worse). Nothing marks content as untrusted,
  and no semantic approval step (close vs edit is a body field, not a URL). Every response is raw, large
  and uncached. A new network listener and trust boundary on the box. We would reimplement Option A's
  checks badly, at a lower level.

### D. Read from the DB only; never write

Keep sync as is, expose only reads. Safe, but does not meet the goal (create/comment). Kept as Phase A,
not the end state.

## Decision

Option A. Issue operations are controller-side tools relayed over NATS. The runner and agent hold no
GitHub credential. Specifically:

1. Tools live in `stacktools` and are handled in `runs.ExecTool` (via a `ToolHandlers` entry owned by a
   new `issuetools` service, as `submit_review` is), so scope is enforced where it already is.
2. Reads are served from the `issue` table (extended with `updated_at`, labels, author, comments), with
   FTS5 search and a live fallback on a miss or stale row. Writes go to GitHub first, then update the local
   row in the same handler (write-through).
3. The `issues` webhook becomes a reconciler: it applies an event only if GitHub's `updated_at` is newer
   than the row's, so our own writes do not echo.
4. Issue state and task state stay separate, linked by `task.issue_id`. No automatic mirroring.
5. All issue text returned to an agent is wrapped and labelled as untrusted data.
6. Close and delete need an owner approval question when requested by an agent, using the existing
   approval path with new ops.
7. Phase A (read tools) needs no permission change. Phase B (writes) requires the owner to raise the Apps'
   `issues` permission to `write`; existing Apps need an owner click to accept it on GitHub.

## Consequences

- No new credential surface. A prompt-injected agent can at worst post comments or edits in its own
  project's repos (Phase B), and cannot close or delete without the owner.
- The `issue` table grows (migration) and the webhook handler changes behaviour (compare-and-set). Both
  are covered by tests first, per the plan.
- Raising the permission is an owner action per App on GitHub (installation permission upgrade). Until
  accepted, GitHub answers 403 on writes; the tools must surface that as a clear "owner must approve the
  new permission" error, not a retry loop.
- Text written by an agent appears on GitHub under the App's identity, visibly marked as from wrangl.
- Live fallback consumes App rate limit; bounded by per-project limits (spec section 7).
- Not covered: pull requests (excluded), issue assignees/milestones/projects, reactions, locking.

## Open questions

Listed at the top of the spec; the owner's answers may amend this ADR.

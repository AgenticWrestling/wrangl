# wrangl — M4: Work Stack, Projects/Repos/Issues, GitHub Tokens, the Work Loop

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Spec:** `docs/superpowers/specs/2026-10-31-wrangl-design.md` §5 (work-stack rules), §6, §8, §10

**Goal:** Runners do real work. A queued task subtree is assigned to an idle runner. The runner
clones the repo, drives the agent (as `agent`, in a PTY) through the subtree's leaves in rank
order, makes a checkpoint commit per leaf and pushes the branch. Output is live (PTY replay)
and searchable (FTS). The work stack is fully editable from the CLI. GitHub is faked (owner
decision, 2026-10-31); the real GitHub App client is written and tested against a fake HTTP
server.

## Decisions

1. **The `stack` service is the only writer of task rows.** It covers create (with position),
   edit, move, precursors, split, queue, complete/reopen/cancel/delete, and the agent's `next`
   and `complete`. Every write records a `task_event`. A container's state is recomputed up the
   ancestor chain after every child change.
2. **The queue resolves the template and repo onto the run** (`run.template_id`,
   `run.repo_id`). Effective value: the task's own, else the nearest ancestor's, else the
   project default. For repo, the fallback is the project's only linked repo. A subtree with
   explicit repos that differ from the root's effective repo is refused.
3. **Assignment** (scheduler):
   1. Order queued runs by project priority, root rank, then age.
   2. Skip runs with no ready leaf.
   3. Claim an idle, uncordoned runner on the run's template at its current version
      (`busy`, `idle_since` cleared), then send `cmd.assign`.
   4. On failure, revert both the run and the runner.
4. **Completion in M4: a turn that exits 0 completes its leaf.** The MCP `task_complete` tool
   and the nudge-then-ask rule come in M5. The runner reports completion through
   `wr.stack.<r>.complete`.
5. **Git credentials never reach the agent.**
   - The daemon (root) clones and pushes, with the installation token passed as an
     `http.extraheader` through `GIT_CONFIG_*` environment variables, so it never appears on a
     command line or on disk.
   - The worktree is then chowned to `agent`.
   - The agent and the checkpoint commits run as `agent`, with no token.
6. **M4 ends with a push.** The run succeeds with `pushed` and `head_sha` recorded, and leaves
   stay `in_review`. Opening the PR, review, and merge to `done` are M6. If open leaves remain
   but none are ready, the runner pushes what it has and the run becomes `blocked`; you re-queue
   it once the precursors are done. Automatic continuation comes with the M6 PR flow.
7. **Output.**
   - Raw PTY bytes go to `wr.pty.<r>.<run>` (JetStream `WR_PTY`), which is what `run attach`
     replays.
   - ANSI-stripped lines go to `wr.evt.<r>.<run>.log` and are ingested into `log_line` + FTS5.
   - Run state goes to `wr.evt.<r>.<run>.state`.
   - Every ingest checks that the run belongs to the runner named in the subject.
8. **GitHub** sits behind a `gh.API` interface: repo lookup (installation, default branch,
   clone URL), installation tokens and issues. `gh.Client` is the real App client (RS256 JWT,
   installation tokens scoped to one repo with `contents:write`, `pull_requests:write`).
   `gh.Fake` is used by the e2e test, and gives `file://` clone URLs so pushes land in a local
   bare repo.

## As built (2026-10-31)

M4 is complete. Every task below is done. Findings and deviations:

- **Two more sqlc quirks (6, 7), plus guards.**
  - Quirk 6: `sqlc.arg()` inside `CASE WHEN … IN (…)` is left as literal text, a runtime
    syntax error.
  - Quirk 7: untyped (`interface{}`) parameters become `NULL` when unset, so `GET /runs/{id}`
    returned 404 for a run that existed.
  - Both were fixed, and `db_test.go` now scans the generated code for unrewritten macros and
    untyped parameters.
- **`blocked` is terminal.** The lock and active-run queries treated a blocked run as active,
  so its subtree stayed locked and couldn't be re-queued. `blocked` now sits with
  succeeded/failed/cancelled everywhere.
- **No `s.DB.W` inside a transaction.** The issue link wrote through the single writer
  connection while a transaction held it, which would deadlock. It's now a query run inside
  the transaction.
- **Bounded shutdown.** Final run states publish with a 30 s limit, so a daemon stopping with
  no controller can't hang. The lost-run sweep covers anything that never arrives.
- **User-data installs git** if the AMI lacks it. Runners clone and push with git.
- **Not exercised against real services.** GitHub (`gh.Client` is tested against an httptest
  GitHub that checks the JWT signature and token scoping), Claude Code (a shell-script agent
  stood in), and real `mise`. `docs/wrangl/INSTALL.md` §11 lists what to watch on the first
  real run.
- **e2e coverage** (`internal/e2e/work_test.go`):
  - project, repo (fake GitHub → local bare repo) and issue sync
  - a subtree with a same-run precursor and an outside precursor: blocked run → push → re-queue
  - issue-to-task, cancel (agent killed, leaf reopened), and a crashing agent (exit code
    recorded)
  - logs filtered and searched, PTY replayed, workspace diff/files/cat
  - the secret not committed, the runner idle after each run
  - the CLI driven against the same controller
  - 5/5 passes under `-race`

## Tasks

- [x] T1 Migration `00004` (`repo.clone_url`; `run.template_id`, `run.repo_id`,
  `run.session_uuid`, `run.pushed`; `project.auto_queue`) + queries.
- [x] T2 `stack` service + tests: positions, cycles, scope, container state, split, queue checks.
- [x] T3 `gh`: API interface, real App client (tested against an httptest GitHub), Fake.
- [x] T4 Projects/repos/issues service.
- [x] T5 Assignment in the scheduler; run-event ingest (state/log); stack RPC for runners;
  cancel; diff/tree/file inspection.
- [x] T6 Runner work loop: clone, branch, agent files, PTY turns as `agent`, checkpoint commits,
  push, cancel, inspection handlers.
- [x] T7 API + client + CLI for `project`, `repo`, `issue`, `task`, `run`, `logs`.
- [x] T8 e2e: project + repo (fake GitHub, local bare repo) → task tree with a precursor →
  queue → assign → fake agent works the leaves in order → commits pushed → logs searchable →
  PTY replay; a cancel test.
- [x] T9 `docs/wrangl/INSTALL.md`: every provisioning step, from AWS to the first run.

# 0012. Multi-repo projects: a writing repo per task, read access to the rest

Status: **Accepted 2026-11-14, built** (stages 1 to 4) on the owner's brief (section 2). The defaults in section 9 are mine; change
any of them before the stage that uses it is merged.

## 1. What exists today

- A project can link several repos (`repo.project_id`, unique per project). `wrangl repo link|list|unlink`.
- `task.repo_id` is nullable and inherited from the nearest ancestor. `run.repo_id` is fixed when the run
  is queued, and a run works in one git worktree of that repo and opens one PR (`pull_request.repo_id`).
- When nothing sets a repo, queueing succeeds only if the project has exactly one linked repo; otherwise it
  is **refused** ("no repo: set one on the task or an ancestor"). Queueing a subtree whose tasks resolve to
  different repos is also refused.
- The scheduler already skips a queued run whose `repo_id` is NULL.
- The agent's `task_create` tool has no `repo` argument, so an agent cannot say where a task it discovers
  belongs. There is no `task edit --repo`, and an agent has no list of the project's repos.
- A run can only see its own repo. Nothing gives it the other linked repos to read.

## 2. The brief

1. A task has a **primary writing repo**. It can **read from any repo linked to the project**.
2. An agent that creates a task can give its target repo.
3. A task with no target repo **stays in the queue until the owner picks one**.

## 3. Writing repo

**Resolution order**, first hit wins: the task's own repo, the nearest ancestor's, the project's **default repo**
(new, optional, `project.default_repo_id`), the project's only linked repo, none. "None" means *needs a repo*.
A research task still resolves to the notes repo first, as today.

**One run, one writing repo.** The runner, the branch and the PR stay one-to-one with a repo. Queueing a task or
a subtree whose leaves resolve to several repos no longer refuses: it creates **one run per repo**, each taking only
the leaves that resolve to its repo. Precursors still order the work across runs, so a leaf in repo B that needs a
leaf in repo A makes B's run wait ("waiting: precursors", which exists) until A's leaf is done. Overlapping
subtrees are still refused.

## 4. A task with no repo waits for the owner

- Queueing succeeds. The run is created with `repo_id` NULL and the status `waiting: needs a repo`; the scheduler
  already leaves it alone.
- A new question kind, **`needs_repo`** (built like `unplaceable`: migration, one open item per run, Inbox
  rendering, auto-closed when resolved), appears in the Inbox. Its options are each linked repo, "link another
  repo" (opens the project's repo sheet), and "cancel the run".
- Picking a repo sets it on the task. A second choice sets it on the whole subtree (the ancestor that lacks one).
  The run then takes the repo and is scheduled normally.
- If every task would resolve the same way, nothing changes: projects with one repo never see this item.
- Tasks that are not queued are untouched. A backlog filter "needs repo" shows them, and a task row shows a
  warning chip only when the project has more than one repo.

## 5. Agents choose the repo

- `task_create` and `task_update` get an optional `repo` (`owner/name`, must be linked to the project). An
  unknown repo is an error that lists the linked ones. An agent may set it only on tasks it creates or in its own
  subtree, as with other edits.
- New tool `repo_list`: linked repos with their default branch, a one-line **note** (new `repo.note`, set with
  `wrangl repo link --note` or edited in the web UI: "API server", "web client") and which is the project default.
  `task_get` returns the task's explicit and effective repo.
- When an agent creates a task without a repo, the task inherits as in section 3; if that resolves to nothing it
  waits as in section 4 when queued.

## 6. Read access to the other repos

- Each run gets a read-only checkout of every other repo linked to the project, at its default branch as fetched
  when the run starts, next to its own worktree: `<run dir>/../refs/<run id>/<repo name>` (per run, so concurrent runs of one repo do not share a path). They come from the runner's
  shared mirrors (one bare clone per repo, as today); a mirror created only for reading is a blobless partial
  clone, to keep the first fetch small. The agent is told where they are, and what each one is, in its prompt
  and in `WRANGL_READ_REPOS`.
- **Read-only is enforced, not requested.** Files are mode `a-w`, the worktree is detached (no branch to push),
  and the daemon mints only a `contents:read` token for those repos and never hands it to the agent. Before the
  run ends the daemon checks that every read checkout is clean and records a warning on the run if not.
- A project setting `read_repos` (default `all`, or `none`) turns it off for a project whose repos are large.
  At most 8 read repos per run; beyond that the repo list is cut with a note.
- A repo whose owner has no App uses the PAT fallback for its read token, as for writes (`github.pat_file`).
- Plan and review runs read the same way, so a review of a PR in repo A can look at repo B's callers.

## 7. Owner surfaces

CLI `task create|edit --repo owner/name` (`--repo ""` clears), `project edit --default-repo`, `repo link --note`.
MCP: the REST bridge picks up the new endpoints. Web: a repo chip and picker on the task page, the resolved repo on
the queue sheet (editable before queueing), a default-repo selector and notes on the project's Repositories card, the
"needs repo" backlog filter, and the Inbox card.

## 8. Other single-repo assumptions to audit

- **Primary sessions (ADR 0011):** a project has one worktree today. With several repos the session starts in
  the default repo (or the first linked) and the others sit beside it under `<dir>/<slug>/`.
- **Diffs and review cards, Smart Merge, auto-merge:** keyed by PR, so by repo already; verify.
- **GitHub issues:** synced per repo already; `task_create` from an issue sets that repo.
- **`wrangl task split` and the plan run:** proposals can carry a repo per subtask.
- **Costs, templates, runners:** unaffected.

## 9. Defaults chosen (change these early)

1. A project **default repo** exists and is optional; unset means "ask".
2. A subtree spanning repos is **split into one run per repo**, not refused.
3. Read access defaults to **all linked repos**, capped at 8.
4. Choosing a repo in the Inbox offers "this task" and "this whole subtree".

## 10. Staging

1. **Resolution and the wait.** The resolver, `project.default_repo_id`, queueing without a repo, per-repo run
   splitting, the `needs_repo` item.
2. **Agent and owner surfaces.** `repo` on `task_create`/`task_update`, `repo_list`, `repo.note`, CLI, MCP, web
   picker, chips and filter.
3. **Read access.** Read-only checkouts on the runner, read tokens, the prompt context, the clean check,
   `read_repos`; plan and review runs; the primary-session layout.
4. **Audit and docs.** Walk section 8, tests across the whole path with a two-repo fixture, the projects and
   tasks guides, the wrangl skill.

## 11. Audit result (stage 4)

Walked section 8 against the built code, with the end-to-end test `TestMultiRepoProjectEndToEnd` (a mixed milestone, a
`needs_repo` item, a read of the sibling repo, one PR per repo):

- **Diffs, review cards, Smart Merge, auto-merge:** all key off the pull request (`pull_request.repo_id`, found by
  repo plus head branch) or the run's `repo_id`, so they were already per repo. A run with no repo has no diff and
  says so.
- **Branch names:** one finding. Several runs of one subtree all took attempt number n+1, so two of them could use the
  same branch name; the repo-less run, once given a repo another run had pushed to, would have collided. Attempts are
  now numbered per run (`wrangl/<sqid>-n`, `-n+1`, ...). One run per task still gets the same name as before.
- **Issues:** syncing is per repo; `issue to-task` sets the issue's repo on the task.
- **Split and plan:** proposals and `task split` carry a repo per subtask (stage 3).
- **Costs, templates, runners:** per run, no repo assumption.
- **Known limits:** `Queue`/`QueueWith` return only the first run when a subtree splits (the others are created and
  visible in the run list); a project's existing single-repo primary-session directory must be moved aside before a
  second repo is linked.

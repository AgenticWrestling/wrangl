# Projects

A project is the unit you plan, queue and review work in. This guide explains what one holds, how the
project page works, what bounds runner launching, what archiving does, and how a project connects to
GitHub. Related guides: tasks, runners and scheduling (in this directory, as they are written).

## 1. Concepts

A project groups a ranked tree of tasks, the GitHub repos those tasks work on, and the settings that
decide how its work is run and merged. Everything else (runs, pull requests, Inbox items) hangs off a
task, so it belongs to the project the task is in.

| Setting | CLI flag | What it does |
|---|---|---|
| **slug** | the argument to `project create` | The permanent id used in URLs (`/projects/<slug>`) and on the CLI. Lowercase letters, digits and `-`, at most 40 characters, starting with a letter or digit (`controller/projects/projects.go`, `slugRe`). It cannot be changed. |
| **name** | `--name` | Display name. Defaults to the slug. |
| **description** | `--description` | Shown in the web app as "Goals & definition of done" (markdown). The first line is the one-line summary on the project page. |
| **default template** | `--template` | The runner template used by a task that names none. It is also the one template a new project may launch runners of (section 4). |
| **plan template** | `--plan-template` | The template used for plan runs (`task split --agent`, "Plan with agent"). Falls back to the default template (`controller/plans/plans.go`). |
| **agent** | `--agent` | The project's default agent definition (`wrangl agent list`). Empty clears it. A per-run `task queue --agent` overrides it. |
| **priority** | `--priority` | An integer; higher wins when projects compete. Queued runs are ordered by project priority first, then your queue order (`db/queries/run.sql`), and when several projects want the same template the higher-priority one gets launches first. |
| **auto-queue** | `--auto-queue` | A new top-level task (no parent) is queued as soon as it is created (`api/work_tasks.go`). Subtasks are not. |
| **auto-merge** | `--auto-merge` | A PR that passes every gate and is judged low risk by the review run is merged without asking you. Anything else becomes an Inbox question. |
| **auto-resolve** | `--auto-resolve` | When auto-merge meets a conflicted PR it queues Smart Merge (an agent merges the base branch in) instead of asking you. Default off. |
| **citation audit** | `--citation-audit` | For research PRs, an extra read-only run re-fetches every cited URL; unverified citations block auto-merge. |
| **notes repo** | `--notes-repo owner/name` | A linked repo that holds research reports under `research/`. It must already be linked to the project; empty clears it. |
| **resource need** | `--need vcpu=N --need ram=4GiB --need disk=10GiB` (repeatable; `ram=` clears) | Default resource need for the project's tasks, overriding the global default (`wrangl placement`). Recorded only; placement does not use it yet. |
| **allowed templates** | `--allow-template T` (repeatable; replaces the list; empty clears) | Other templates whose runners this project's runs may use besides their own, usually `host`. A free allowed runner is used first; the autoscaler launches only when none has room. See the scheduling guide. |

Changing a flag with `project edit` changes only that setting.

**Custom tools** (per-project MCP servers and HTTP tools with credentials) are not built. The project
settings page shows a "Custom tools - coming soon" placeholder. The design is
[ADR 0003](../adr/0003-project-custom-tools.md), accepted but not implemented.

## 2. The project page

`/projects` is the list: a table with runner, in-progress, queued, backlog and inbox counts per
project. A row expands to show an about line, the runners working now, recent activity, and
Open / Comment / Edit / Archive. "New project" opens an inline form. Archived projects are collapsed
under the table with a Restore button.

`/projects/<slug>` is the page you work in. From the top:

- **Title switcher.** The title is a button; it opens a menu of projects, "All projects" and a new-project
  entry. (On desktop the five most recently updated projects are also listed in the left nav.)
- **About line** and, if archived, an "archived" tag.
- **Action strip.** Linked repos (each a link to GitHub, or "no repos linked" pointing at the settings
  page), an auto-merge indicator when auto-merge is on, **Edit goals and repos** (opens
  `/projects/<slug>/settings`), the **Comment** button (project chat, below), and **Archive** (it asks
  first).
- **Needs you.** The project's open Inbox items (questions, task reviews, PRs, tool-failure notices).
  Hidden when there are none.
- **Bar.** An *In progress* filter chip (only leaf tasks being worked on now), the **runners chip**, and **＋** for a new task.
  - The runners chip counts runners that have a session for this project; its popover lists each one
    with its state, template and the tasks it is running. A runner that also hosts another project's
    session is tagged "also serving X".
- **In progress** (runs that are assigned or running, with their runner and the leaf being worked),
  **Queue** (queued runs in the order they will be assigned: your reordering first, then tree position,
  then first in first out) and **Backlog** (everything else, with "show done"). A queued or running
  task's subtree appears once, in its section, not again in the Backlog.

Row buttons: **Queue** on open top-level backlog rows (any live task can also be queued from its task
page or `wrangl task queue`), **Dequeue** and up/down on queued rows, **Stop** on running rows, **Queue
all** on the Backlog header. Destructive buttons ask inline ("Yes / Cancel"), never with a browser dialog.

**Settings page** (`/projects/<slug>/settings`): Goals & definition of done (markdown, edited in a sheet
together with name and default template), Repositories (link and unlink), and the
Custom tools placeholder. The older URLs `/tasks?project=<slug>`, `/backlog` and `/project/<slug>`
redirect to these pages.

### Project chat (Comment)

Comment opens a panel (full screen on a phone; `?chat=1` opens it from a link). You are talking to a
**controller agent**: the installed `claude` CLI, run on the controller host, restricted to read-only
tools and the `wrangl` CLI, niced, and told not to build or run long processes
(`controller/agents/agents.go`, `agents/project.go`). It is given the project's tasks, runs, PRs and
repos (the context chip shows the counts) and answers in text.

When it recommends changes it ends with up to five **proposals**, shown as "Approve: ..." buttons. A
proposal changes nothing until you press it, and it is refused if the project changed in the meantime.
The agent is told never to propose deleting, cancelling, terminating, approving or merging. Two slash
commands skip the model: `/queue <id or title>` and `/dequeue <id or title>` act at once (an ambiguous
title offers buttons). Task ids in replies link to their rows. The chat is per project and kept
(`project_thread`); this is a different thing from a task's own "Ask?" thread.

Which agent answers is chosen from Settings → Controller agents (the default is the `claude` CLI on
the controller host).

## 3. Repos and GitHub

wrangl works against GitHub through **GitHub Apps you own**, one per GitHub account (user or
organisation) that owns repos you use. You create each App from the Setup page
(Settings → GitHub accounts → Manage) with GitHub's manifest flow; see
[INSTALL.md section 6](../INSTALL.md#6-github-one-app-per-account) for the steps and the
"Ready / Not installed / Credentials rejected" states. A repo can only be linked if its owner has an
App and the App is installed on that repo.

```sh
wrangl repo link web your-org/your-repo [--branch main]   # branch defaults to the repo's default branch
wrangl repo list [--project web]
wrangl repo unlink web your-org/your-repo
```

- `repo link` asks GitHub for the repo using the owner's App. If there is no App or it is not installed
  on the repo it fails and says so; fix that on the Setup page and retry. Linking a repo that is already
  linked to the project is a conflict. Unlinking keeps history on tasks that pointed at the repo.
- A task works in one repo, set with `task create --repo owner/name` or inherited from its parent.
  A run on a task with no repo cannot be assigned. Runs get a one-repo, per-run installation token that only
  the runner daemon uses; the agent never sees it.
- A run opens one PR for its subtree. Gates, the review run, auto-merge, Smart Merge and the
  conflict state are covered in INSTALL.md section 9.

### Issues

`wrangl issue sync PROJECT` copies the issues of every linked repo into a local table (only newer data
overwrites older, so a sync never clobbers a fresher webhook). Then:

```sh
wrangl issue list --project web [--state open|closed|all]
wrangl issue show owner/name#12 --project web
wrangl issue to-task owner/name#12 --project web [--parent SQID] [--template T]
wrangl issue drift web [--repair]      # issues whose copy differs from GitHub
```

`to-task` creates a task titled `#12 <issue title>` in that issue's repo, with a prompt made of the
issue's URL and body, linked to the issue. Task state and issue state never follow each other.

The `issues` webhook keeps the table current; the reconcile poll is the backstop. **Agents** can use
the same data through MCP tools served by the controller: `issue_search`, `issue_get`,
`issue_comments`, and for work and revise runs `issue_comment`, `issue_create`, `issue_edit`,
`issue_close` (closing only after you approve an Inbox question) and `issue_link_task`. They see only
repos linked to the run's project, and issue text reaches them wrapped as untrusted data. Writes need the
App to have `issues: write` (Apps created before that need the upgrade in INSTALL.md 6.2a). Tool
arguments and limits are in [agents.md](../agents.md#github-issue-tools) and
[ADR 0004](../adr/0004-agent-github-issue-tools.md).

## 4. Runner limits

Runners are a shared pool. There are **no per-project instance limits** any more (the old `--limit`, the
"Runner limits" card and the concurrency chip were removed with the legacy launch tick; see the
[scheduling guide](scheduling.md#6-the-autoscaler)). What bounds launching now:

- the template's `max_runners` (an account-wide backstop; the natural way to keep an expensive template such as
  a Mac or an emulator host from being started in bulk),
- the hourly budget, when one is set (`wrangl placement set --budget`), and
- what queued runs actually need: runners are launched only for queued work, packed by resources.

A runner a project's demand caused is still recorded (`runner.launched_for_project`) for attribution, but it
limits nothing. A project reaches a template at all through its default template, a per-task template, or
`--allow-template`. The controller agent's `fleet.launch` proposal is refused, naming `max_runners`, if it
would exceed it.

## 5. Archiving and restoring

Archive a project when it is finished or abandoned. Nothing is deleted.

```sh
wrangl project archive web
wrangl project list [--archived | --all]     # default: active only
wrangl project restore web
```

- **Refused while runs are active.** If any run of the project is assigned, preparing, running,
  needs input, pushing or cancelling (`ActiveRunsOfProject`), archiving fails and lists each run (id,
  state, task, title). There is no cancel-and-archive: let them finish or `wrangl run cancel` them, then
  retry. The reason is that an archived project is inert, and a running run would be orphaned.
- **Queued runs are cancelled** with the reason `project archived`. Restoring does not requeue them;
  queue the tasks again.
- **What stops:** the scheduler assigns nothing and launches no runners for it, auto-queue and
  auto-merge do nothing, new tasks are refused (409 "project ... is archived"), PRs are not reviewed or
  adopted, `fleet.launch` for it is refused, and no new task-review or failure questions are raised.
- **What is hidden:** the project and its tasks disappear from `project list`, `task list`, the Inbox,
  run and PR lists, log search, the Projects table (archived ones sit in a collapsed "Archived
  projects" section), the nav, the Settings project list and the agents' project list. By-name
  lookups still work: `project show`, `task show`, `/projects/<slug>`, `/task/<sqid>`.
- **What is kept:** PRs stay on record (a PR merged or closed on GitHub is still marked so), issue sync
  keeps recording, and PRs are picked up again after a restore.
- **Web app:** the Archive button is on the Projects row and the project page; Restore is in the
  Archived section. API: `POST /api/v1/projects/{slug}/archive` (409 with the run list when refused) and
  `.../restore`.

`wrangl project delete SLUG` is different: it removes the project **and all its tasks**, and is also
refused while a run is active. Prefer archive.

## 6. CLI reference

All commands take `-o table|json|yaml`.

| Command | Notes |
|---|---|
| `project create SLUG [flags]` | Flags as in section 1 (`--name --description --template --plan-template --agent --priority --auto-queue --auto-merge --auto-resolve --citation-audit --notes-repo --allow-template --need`). |
| `project edit SLUG [flags]` | Same flags; only those given change. Refused on an archived project. |
| `project show SLUG` | The settings, including allowed templates and needs. |
| `project list [--archived \| --all]` | Active projects by default; a STATUS column says which are archived. |
| `project archive SLUG` / `project restore SLUG` | Section 5. |
| `project delete SLUG` | Deletes the project and its tasks. |
| `repo link/list/unlink` | Section 3. |
| `issue sync/list/show/to-task/drift` | Section 3. |

A typical start:

```sh
wrangl project create web --name "Web app" --template go-claude --auto-merge
wrangl repo link web your-org/your-repo
wrangl project edit web --description "## Goal ..."   # or edit the goals in the web app
wrangl issue sync web
```

## 7. Troubleshooting

| Symptom | Cause and fix |
|---|---|
| **A queued run sits in the Queue with status "waiting: ..."** | The status text names the cause: a missing agent secret (`wrangl agent auth`), a repo without an installed App, a template that does not allow the agent, or no capacity. Read it with `wrangl run show ID`. See the scheduling guide for the full list. |
| **Queued, nothing launches, status mentions no capacity** | Check the template's `max_runners` and live count (Runners page), whether its current version failed to apply, the hourly budget (`wrangl placement show`), and whether the project is archived. After 15 minutes the run raises a "can't be placed" Inbox item naming the cause. |
| **Max is 2 but only 1 runner starts** | The scheduler launches only for demand that the existing runners' free capacity cannot hold: a second runner starts only when queued runs do not fit the first one (by sessions, and by resource needs once they are set). |
| **Another project's work is first** | Priority: higher `--priority` is served first, both in assignment and in launches. |
| **`repo link` fails: "GitHub is not configured" / "not installed"** | The repo's owner has no App yet, or the App is not installed on that repo. Setup page → add the account → create/install the App → pick the repo. |
| **`project archive` is refused and lists runs** | Wait for the listed runs or `wrangl run cancel ID`, then archive again. |
| **A task I created is not queued** | `--auto-queue` queues only new top-level tasks, and only those created after it was turned on. Queue others with `wrangl task queue`. |
| **A project vanished from the lists** | It is archived: `wrangl project list --archived`, then `project restore`. |
| **Creating a task fails with 409 "archived"** | Restore the project first. |
| **The project chat does not answer** | It runs a controller agent on the controller host (Settings → Controller agents; by default the `claude` CLI there). Check that the CLI is installed and logged in on that host, and the controller log. |
| **`--notes-repo` is rejected** | Link the repo to the project first (`repo link`). |
| **A conflicted PR is not fixed by itself** | `--auto-resolve` is off by default; use `wrangl pr resolve ID` or turn it on. |

## See also

[Tasks](tasks.md) (what runs inside a project), [Runners](runners.md) (what the limits cap), [Task scheduling](scheduling.md) (how queued runs get slots), [index](../README.md).

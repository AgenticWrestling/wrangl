# ADR 0005: Projects as the primary lens (routes, counts, runners, shared components, project chat)

- Status: **Accepted, implemented** (routes, counts, project page, shared components and project chat)
- Task: 24biFM, under gmy0K3 (Projects as the primary lens). Later subtasks follow this note.
- Builds on: the Tasks page (`web/templates/tasks.html`, `web.go` `tree`), the Inbox card (`icard`), the Logs/Fleet expandable rows, the ask-agent/thread machinery (`controller/agents`, `task_thread`), `run.queue_rank` and `POST /api/v1/runs/queue-order`, project archive (migration 34), per-project template caps (migration 25).
- Caveat: the mockup (claude.ai artifact YbsRE5M4dVGTMCAwJh6LPG) needs a logged-in session and could not be fetched from the agent environment. This note works from the task description of gmy0K3; the implementer of the first UI subtask should check it against the mockup at desktop and phone widths and amend this ADR where they differ.

## Context

Today "Tasks" is a page per project chosen with `?project=slug`, "Project goals & repos" is a separate `/project/{slug}` page, and the five recent projects are a `projbar` row on the Tasks page. The goal is to make the project the unit of navigation: a Projects list, a project page that holds queue/backlog/inbox/runners for that project, and a project-level chat.

What exists and constrains the design:

- **Server-rendered, Datastar-driven UI.** Pages are `h.page` -> `h.data(view)` -> template `main-<view>`; `/ui/live` (SSE) re-renders regions; `/ui/act/{action}` is one big switch (`web.go` `act`) that calls services in-process and patches the page. Expanded details are kept in a per-session set so they survive live patches (last commit).
- **Queue.** `Stack.Queue` creates a `queued` run for a task. `run.queue_rank` (REAL, nullable) orders queued runs; `POST /api/v1/runs/queue-order` sets it, and the Tasks page already has a sortable Queue list. **There is no dequeue**: a queued run can only be cancelled (`run-cancel`). "Dequeue" is therefore cancelling a *queued* run, with a name and a guard (only `state='queued'`).
- **Runners.** `run.runner_id` ties a run to a runner; runners are not owned by projects. `runner.launched_for_project` records who caused a launch, but a runner serves any project's runs (up to `template.max_sessions` concurrent sessions). So "runners for a project" = runners that host a non-terminal run whose task is in the project. Shared = that runner also hosts active runs of another project.
- **Ask-agent.** `agents.Service.Ask(sqid, agent, question)` stores a user message in `task_thread`, starts a boxed `claude -p` on the controller (nice, 2 concurrent, turn/time limits, tools restricted to `wrangl` CLI + read tools, delete/cancel/approve denied), resumes the CLI session per (task, agent), and parses a fenced `wrangl-proposals` block into at most 5 one-click proposals from a closed allowlist (`Actions`, run in-process by `Apply`). Proposals expire when the task subtree's `SubtreeRev` changes. The context ("brief") is the task subtree as markdown, capped at 24 KiB.
- **Inbox card.** `icard` is a self-contained template driven by an `inboxItem` (actions, confirm, free-text reply, busy indicators) built by `inboxItems`, which currently takes all questions and open PRs.
- **confirm().** Used in fleet.html, run.html, task.html, inbox.html, project.html, setup.html (`data-on:click="confirm('..') && @post(..)"`), i.e. the browser dialog the task wants gone.

## Decision 1: Routes

| Route | Behaviour |
|---|---|
| `GET /projects` | Projects list (counts table, expandable rows, New project form, archived collapsed). |
| `GET /projects/{slug}` | Project page (strip, Needs you, In progress, Queue, Backlog). `?all=1` = show done. |
| `GET /projects/{slug}/settings` (name TBD; may stay one page) | Edit goals & repos: the current `/project/{slug}` page, reached from "Edit goals & repos". Keeping it a separate route avoids rewriting that form in the first PR; it is only *linked* differently. |
| `GET /tasks`, `/tasks?project=x`, `/backlog` | 302 to `/projects/{x}` (or `/projects` when none; `#backlog` kept for `/backlog`; `?all=1` and `?now=1` map to `?all=1` and `#in-progress`). Never removed. |
| `GET /project/{slug}` | 302 to `/projects/{slug}/settings`. |
| `GET /` | 302 to `/projects` (was `/tasks`); on a phone-sized first visit nothing special: the last visited project is remembered client-side and used by the nav link. |
| `GET /task/{sqid}` etc. | Unchanged. Task pages gain a breadcrumb to their project. |

Left nav and phone tab bar: "Projects" replaces "Tasks"; the five most recent projects stay as `subnav` (links to `/projects/{slug}`). Phone: a project pill at the top opens a menu of projects plus "All projects". Active-nav matching must treat `/projects*` and `/task/*` as the Projects section.

Alternative considered: a path-less project via a cookie/`?project=`. Rejected: not linkable, and the redirect table above would be needed anyway. Slugs are already unique and URL-safe (`project-new` validates them).

## Decision 2: Counts and live data

The Projects list needs, per project: runners (active), in progress, queued, backlog, inbox. Options:

- **A. One aggregate SQL query** (`ProjectCounts`) returning a row per active project, computed with correlated subqueries over `run`/`task`/`runner`, plus the inbox count taken from the same source as the Inbox page. **Chosen.** One query, no N+1, usable for the sidebar badge too.
- B. Reuse the per-project `tree()` for each project and count in Go. Rejected: builds full trees (and attention maps) for every project on every SSE patch.
- C. A denormalised counts table maintained by triggers. Rejected: stale-count bugs outweigh the saving at this scale (tens of projects).

Definitions (to be fixed in the SQL and tested): *in progress* = leaf tasks under a run in `assigned/preparing/running/needs_input/blocked/pushing`; *queued* = runs `queued`; *backlog* = open tasks with no active or queued run, top-level count (what the Backlog section shows); *inbox* = open questions + PRs awaiting review for the project; *runners* = distinct `runner_id` over the active runs of the project.

Inbox per project: `inboxItems` already derives each item's task; add a `project` field and a `ProjectInbox(slug)` filter rather than a second builder. The project page "Needs you" strip renders those items with the same `icard`. Acting goes through the existing actions; after an act the `/ui/live` patch re-renders both the strip and the counts (the Bus topics `task`, `question`, `pr` already trigger live refresh; the project page subscribes to the same set). No new endpoint is needed for the strip.

New HTTP surface is small and stays on `/ui/act/*`: `task-dequeue`, `queue-all` (confirmed, bulk: queues every backlog root of the project), `queue-move` (up/down, wraps `queue-order`), `project-chat-*` (below). Public API gains `POST /api/v1/tasks/{sqid}/dequeue` and `GET /api/v1/projects/{slug}/summary` (counts + runners) so the CLI and chat agent can use the same data (`wrangl project show -o json`).

## Decision 3: Runners per project

Never assume one runner. Query `ProjectRunners(project)`: runners with at least one non-terminal run for the project, each with its sessions (run id, task, state) and `shared_with` = other projects' slugs among that runner's active runs. The chip says "runners N active" (N = distinct runners); the popover/list shows every runner with its project sessions and a tag "also serving X, Y" when shared. A runner with only another project's sessions is not listed. Runners that were launched for the project but are idle (`launched_for_project`) are listed as "idle" so a fresh launch is visible. The concurrency chip keeps showing the template caps (`project_template_limit`). Scheduler changes (JZ1IiM) are out of scope; this only changes display, and the list must cope with ten or more rows (scrollable, grouped by template).

## Decision 4: Shared UI components to extract (small refactors, each its own PR before or with the page that needs them)

1. **Inline confirm** (`{{template "confirm"}}`): a button that on first click swaps itself for "Sure? [Yes] [Cancel]" in place, driven by a Datastar signal (`confirm<key>`); the destructive `@post` fires only on Yes; auto-reverts on blur/after ~5 s. Replaces every `confirm()` listed above, including `onsubmit="return confirm(...)"` in setup.html (turn that into a signal-gated submit). `inboxAction.Confirm` keeps its field but renders through the component. A test greps templates to forbid `confirm(`.
2. **Expandable row**: extract the Fleet/Logs `<tr class="expandable" data-on:click ...>` pattern (keyboard-accessible, ignores clicks on links/buttons, state in a signal that survives live patches) into a template/func used by the Projects table and, as cards, on phone.
3. **Inbox card**: `icard` already works standalone; the change is making `inboxItems` filterable by project and giving `icard` a compact modifier for the strip. No fork of the template.
4. **Flash-on-act**: `act` already knows the target (`sqid`/`id`). A tiny helper `flash(w, dom)` patches a `flash` class onto the row (CSS animation ~1 s) after the fragment merge; used by Queue/Dequeue/Stop/reorder. Rows need stable DOM ids (`t-<sqid>`, `run-<id>`), which `tasklist`/`runentry` lack today; adding them is part of that PR.
5. **Row actions**: Queue / Dequeue / Stop / up / down buttons as one partial used by Backlog, Queue and In progress rows. Drag reorder keeps Sortable + `queue-order`; up/down is the keyboard and phone path.

## Decision 5: Does project chat reuse the ask-agent/thread machinery?

Options:

- **A. Reuse, generalised from task to a scope (task or project). Chosen.** Add a nullable `project_id` to `task_thread` (making `task_id` nullable, with a CHECK that exactly one is set), a `Service.AskProject(slug, agent, question)`, and a `brief` builder for projects. Everything else is shared: agent registry and default, slot semaphore and `nice`, timeouts, session resume (`ThreadSession` keyed by scope + agent), Recover, the `thread` bus topic, `wrangl-proposals` extraction, `Apply`, the thread view template and its running/error states.
- B. A separate project-chat service and table. Rejected: duplicates process boxing, concurrency cap, session handling, proposal parsing and rendering; two places to keep the deny-list in sync.
- C. Make the project chat a thread on a hidden root "project task". Rejected: pollutes task trees, ranks, counts and the CLI with a fake task; archive/delete semantics get odd.
- D. Run the project chat on a runner like normal work. Rejected: slow to start, costs an instance, and the point is a fast read-mostly assistant; the controller agent is already boxed for that.

How it runs: on the controller exactly like Ask, one `claude -p` per message, `--resume` of the project's session, so it shares the two-slot cap with task asks (documented consequence below).

Context it reads (`projectBrief`, capped like the task brief at 24 KiB, trimmed in this priority order): project name, description, goals; repos; open Inbox items; in-progress runs (task, runner, state); the queue in rank order; backlog roots (title + sqid, then prompts if room); recent finished runs and open PRs (state, checks). Anything beyond the brief is fetched by the agent with the `wrangl` CLI (`task tree`, `pr list`, `run list` with `-o json`). The UI shows a *context chip* ("12 tasks · 3 runs · 2 PRs · 4 repos") computed from the same brief builder, so what the chip says is what the agent got; it flags "trimmed".

Proposals and approval: unchanged mechanism. The closed `Actions` allowlist is extended with project-level verbs: `task.dequeue`, `queue.reorder` (`{"sqids":[..]}`), `task.queue_all` (`{"sqids"?}`). `task.create` already defaults to the thread's project. The reply renders approve buttons through the existing proposal partial. Staleness: `Rev` becomes a project fingerprint (max task/run update marker for the project) instead of `SubtreeRev`; any change expires unapplied siblings, same behaviour as today. `fleet.launch` already works at project scope. Destructive actions (delete, cancel, approve, merge) stay prose-only.

Task ids in replies are linked: a post-render pass turns `` `abc123` `` / bare 6-char sqids that exist in the project into links that scroll to and flash the row (`#t-<sqid>`), or to `/task/<sqid>` if the row is not on the page.

`/queue` and `/dequeue`: handled **before** the model, by the web handler, not by the LLM. `/queue <sqid|title fragment>` and `/dequeue ...` resolve against the project's tasks (exact sqid, else unique case-insensitive title match; ambiguity returns a pick list as proposals); the result is posted as an agent-role message ("Queued abc123") without spending an agent turn, using the same `Stack.Queue` / dequeue code as the buttons. Plain-text asks go to the agent. Slash commands never bypass the allowlist or confirm rules: `/queue` of many tasks needs the inline confirm.

UI: a "Comment" button next to "New task" opens the chat in a right-hand panel on desktop (the project page stays interactive), full screen on phone with a floating chat button; the chat is rendered by the thread template with `scope=project`. The panel is a live region on the `thread` topic so replies and applied proposals update in place.

## Consequences

- Positive: one agent pipeline to secure and test; project chat is mostly a new brief builder, an `AskProject`, a nullable column and a UI panel. The inbox strip, counts and runners list are queries over existing tables, with no scheduler change. Old URLs keep working.
- Negative / costs: `task_thread` migration (nullable `task_id`, new column, CHECK) touches `ThreadSession`, `AddThreadMessage`, `Apply` (which today loads the task to find the project) and `sqlc` queries; thread-view code is generalised from task to scope. The shared 2-slot cap means a long task ask can delay a project comment (the 2-minute slot wait already reports this); raising `MaxConcurrent` is a separate decision. The project brief is a larger and more variable prompt than a task subtree, so quality depends on the trimming order. The counts query is another place that must agree with the Tasks/Inbox definitions (hence tests on the definitions). Dequeue is a new, small server operation.
- Out of scope here: scheduler changes for shared runners (JZ1IiM), keyboard shortcuts, backlog filter, Inbox project filter, health dots, sidebar activity sort (follow-ups).

## Suggested PR order for the follow-up subtasks

1. Inline confirm component + replace all `confirm()` (independent, ships first).
2. Routes and redirects, nav rename, project switcher/pill; `/projects/{slug}` initially renders today's Tasks content.
3. `ProjectCounts` + Projects list (expandable row component, New project form, archived section) and tests.
4. Project page strip, runners chip/list with shared tags, Needs you strip (project-filtered `icard`).
5. Queue verbs: dequeue endpoint, Queue all, up/down, flash-on-act, Stop.
6. Project chat: migration, `AskProject`, brief + chip, slash commands, panel UI, new proposal actions.
7. e2e tests, SKILL.md, README text and screenshots (also updated incrementally per PR).

## Addendum: how project chat was built (task i3Shwa)

Decision 5 held (reuse the machinery) with one deviation in the storage: instead of making `task_thread.task_id` nullable (a table rebuild plus changes to the generated queries), project chat has its own table, `project_thread` (migration 35), with the same columns. Everything else is shared in `controller/agents`: the agent registry, the boxed execution (`Service.execute`, extracted from the task path so both use the same slot cap, secrets and timeout), session resume, `wrangl-proposals` parsing and `run`. Project-specific code is in `project.go`: `AskProject`, `projectBrief` (project, repos, live runs with the queue in rank order, open PRs, open tasks; 24 KiB cap, with the chip counts), `ApplyProject` (proposals expire when the project fingerprint changes), `/queue` and `/dequeue` handled before the model, and task ids in replies returned as structured `tasks` links. New proposal actions: `task.dequeue`, `queue.reorder`. API: `POST /api/v1/projects/{slug}/ask`, `GET /api/v1/projects/{slug}/thread` (messages plus `context` chip), `POST /api/v1/project-thread/{id}/apply/{n}`.

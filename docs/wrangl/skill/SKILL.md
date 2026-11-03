---
name: wrangl
description: Use when the user wants to manage work in wrangl, their coding-agent fleet controller (ctrl.example.com): create/edit/move/split/queue tasks, projects and repos, watch runs and logs, answer agent questions, review PRs, or manage runners and templates. Triggers - "wrangl", "task sqid", "queue this", "backlog", "runner", "fleet".
---

# wrangl

wrangl is a controller that queues coding-agent **runs** onto **runners** (EC2 boxes, plus the
controller host itself as a permanent runner) and tracks work as a hierarchical **task** tree
inside **projects**. Use the `wrangl` CLI; on the controller host it talks to the running
controller over `~/.config/wrangl/config.yaml` (a unix socket), so no auth is needed there.
Elsewhere set `--controller https://ctrl.example.com` (Cloudflare Access applies) or
`WRANGL_CONTROLLER`.

Every command takes `-o json|yaml|table`; use `-o json` when you need to parse output.
Tasks are addressed by **sqid** (short id shown in `task tree`/`list`). Run `wrangl <cmd> --help`
for exact flags; this skill is the map.

## Tasks (the work stack)
- `wrangl task tree [--project P]` - ranked tree; `task list --project P --state S --parent SQID`.
- `wrangl task show SQID` - precursors, runs, history.
- `wrangl task create PROJECT --title T [--prompt TEXT | -f prompt.md] [--parent SQID] [--type TYPE] [--target-date YYYY-MM-DD] [--needs SQID...] [--repo owner/name] [--template NAME] [--after|--before SQID | --first | --last] [--issue owner/name#N]`
  The prompt is what the agent is told to do: make it self-contained (goal, constraints, done criteria).
- `wrangl task edit SQID [--title] [--prompt|-f] [--type] [--target-date YYYY-MM-DD]` (an empty `--target-date` clears it).
- `wrangl task move SQID [--parent SQID | --root] [--after|--before SQID | --first | --last]` - reparent or rerank.
- `wrangl task split SQID --title A --title B [--chain] [--copy-prompt]` or `-f outline.md` (one subtask per line, indent to nest); `--agent [--guidance TEXT]` queues a plan run that proposes the split for you to accept.
- `wrangl task needs add|rm SQID PRECURSOR` - ordering dependencies between tasks.
- `wrangl task ask SQID "question" [--agent NAME] [--wait]` - ask a controller agent (a light Claude on the controller host, with this skill) about a task and its subtree; answers collect in a thread on the task (`wrangl task thread SQID`, or the "Ask?" button); the agent may attach up to 5 proposed actions, approve one with `wrangl task apply MSGID N`. Agents are registered in Settings > Controller agents.
- `wrangl task queue SQID` - queue a work run for that subtree (nothing runs until queued, unless the project has auto-queue).
- `wrangl task complete SQID [-m MSG]` (leaves only; parents complete when their subtasks do), `task reopen`, `task cancel` (with unfinished subtasks), `task delete` (whole subtree - confirm with the user first).

## Task types
`code` (default), `bug` (reproduce, failing test first), `ux` (mockups + prototype link; the owner reviews visually), `architecture` (an ADR; the owner reads before merge), `docs`, `research` (a report with sources; non-code knowledge work), `review` (findings, no diff), and the planning containers `release` > `milestone` > `feature` (a target date, a progress rollup, drawn bold; children of a planning task default to `code`). `spec` is an old alias of `docs`.
A release whose work is all done waits in review until the owner ships it (`wrangl task complete RELEASE`, or "Ship it" on the task page). Runs on a planning task work through its subtree; they do not do the container itself.
Agents hand back non-PR results with the `submit_deliverable` tool (image, prototype link, link, markdown report); the owner sees them on the task page and approves or requests changes from the inbox.

## Projects and repos
- `wrangl project list|show|create|edit|delete`; `project create SLUG --name N --template T [--auto-queue] [--auto-merge] [--priority N] [--description ...]`. Goals / definition-of-done markdown is edited on the web project page (`/project/SLUG`).
- `wrangl repo link PROJECT owner/name [--branch B]` (the GitHub author App must be installed on the repo), `repo list`, `repo unlink`.
- `wrangl issue sync|list|show|to-task` - pull GitHub issues in and turn them into tasks.

## Runs, logs, questions, PRs
- `wrangl run list|show ID`, `run attach ID` (read-only terminal, replays from start), `run diff|files|cat ID`, `run cancel ID`.
- `wrangl logs RUN [-f] [--grep RE] [--since 15m] [--stream agent|runner]`; `wrangl logs search QUERY` (FTS5, all runs).
- Agents ask questions and request approvals: `wrangl question list`, `question show ID`, `question answer ID [--option X] [--text ...]`. Check these first when a run seems stuck.
- `wrangl pr list|show`, `pr approve` (merges; needs reviewer App), `pr request-changes` (queues a revise run), `pr close` (tasks reopen).

## Runners and templates
- `wrangl fleet list|show NAME`, `fleet launch --template T [--count N]` (new EC2), `fleet drain|cordon|terminate NAME`.
  Launched runners are reaped after the template's idle timeout (30m default); the controller host runner (`host-*`, template `host`) is permanent and cannot be terminated.
- `wrangl template create -f file.yaml | edit | ...` - mise tools, setup script, agent command. Examples in `docs/wrangl/templates/`.
- `wrangl fleet enroll-token --template T` then `wrangl runner bootstrap --controller URL --token ...` adopts an existing machine; add `--host` to enrol the controller host itself under the current user (systemd --user unit).
- Web terminal: Runners view -> Terminal (`/runner/NAME/term`): a tab per agent session plus "+ Shell" (fish if installed, else bash). Shell tabs open in control; run tabs are watch-only until "Take control". The first shell on a runner installs bat, lsd, dust, duf, fd, ripgrep and tldr.
- The controller host is a permanent runner (`host-*`, pinned first in the Runners view, never reaped); enrol it with `wrangl runner bootstrap --host`.

## Web app (PWA)
Inbox / Backlog (project buttons, `+` to create) / Runners / Settings (text size, push, Setup: GitHub Apps, Projects, templates). Same data as the CLI; use whichever fits.

## Working rules
- Read first (`task tree`, `task show`) before editing or moving; confirm with the user before `delete`, `cancel`, `pr approve`, `fleet terminate`.
- Prefer `-o json` and quote prompts via `-f file.md` for anything multi-line.
- Don't queue broad subtrees without saying what will run and where.

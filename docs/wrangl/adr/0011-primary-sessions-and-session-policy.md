# 0011. Primary sessions per project, a Workers view of every Claude session, and a session policy

Status: **Proposed 2026-11-12.** Not built. The permission lists in section 6 are for the owner to
approve or edit before anything is installed.

## 1. Problem

The controller host runs three kinds of Claude process side by side, and nothing connects them.

- **wrangl run agents** (host runner): queued, gated, costed and visible. Today they run as the owner
  in the real home, with permission prompts bypassed.
- **Interactive sessions** the owner (or a Remote Control session) starts by hand. Some are
  long-lived (three `claude` processes on the host are 6, 9 and 12 days old). They share the owner's
  `~/.claude`, git config, the live checkout and the controller's data directory with the run agents.
- **Subagents** those interactive sessions fan out with the Agent tool. They are invisible to wrangl:
  no task, no review gate, no cost, no slot accounting.

What the history shows (the interactive session of 2026-11-02..12 and runs 16-126):

- Run agents use almost only `Bash` (667 calls), `Write` and the wrangl MCP tools
  (`task_complete`, `submit_review`, `report_status`). They are cheap to constrain.
- The interactive session made ~2600 `Bash` calls (cd, sed, grep, `python3 -`, `go test`, `git`,
  `wrangl`, `curl` to the local API, `gh`), 11 `Agent` calls (investigation and design, all of
  which could have been tasks) and a handful of `Artifact`, `Monitor` and `Write`.
- 19 of 20 recent wrangl runs succeeded. The one failure was an expired git token, not an
  interaction. The owner's concern, though, is the shared environment: a bypassed agent in the real
  home can edit the controller, the checkout or the other session's files.

## 2. Goals

1. Every active project has one **primary Claude session**, with Remote Control, named after the project.
2. Reaching its terminal is **one click** from the Projects page, and also one command over SSH.
3. **Every** Claude session on the controller and on runners shows in one place, the **Workers** view.
4. Substantial work from an interactive session goes through wrangl tasks, enforced by the harness.
5. Agents on the host cannot touch the owner's home or the controller's data.

## 3. Primary session

**What it is.** A long-lived interactive `claude --remote-control <slug> --name <slug>` per active
project (not archived, repo linked), started in a git worktree of the project's repo on the default
branch (`~/work/primary/<slug>`). It is never the dev checkout. The name is the project slug, so it
reads the same in claude.ai/code, the mobile app and the Workers view.

**How it runs.** wrangl owns the PTY; there is no terminal multiplexer (zellij and tmux are not used).
The controller already owns PTYs for the web terminal (`runner/term.go`, creack/pty, brokered over
NATS), and one more kind of PTY should use the same code. The one constraint is lifetime: the
controller restarts on every deploy, and a PTY owned by it would die with it. So primary sessions live
in a small separate service, the **session host**: `wrangl session-host`, the same binary under its
own systemd user unit (`wrangl-sessions`), which the deploy script does not restart. It starts each
`claude --remote-control <slug> --name <slug>` in the project's worktree, keeps the PTY and a bounded
scrollback ring, and serves a unix socket (`~/.local/share/wrangl/sessions.sock`, mode 0660, group
`wrangl-sessions`). The controller is a client of that socket, as is the CLI. Reattach sends SIGWINCH so
Claude's TUI repaints, and replays the ring first. The controller's reconciler (starts a session when a
project becomes active or gains a repo, stops it on archive, restarts with `claude -c` and backoff) talks to
the session host; it does not hold PTYs itself. Upgrading the session host is explicit
(`wrangl session-host restart`, which warns that sessions will resume with `-c`).

**Screen state: `charmbracelet/x/vt` (spiked 2026-11-12).** The session host feeds each PTY's output
through `vt.SafeEmulator` as well as the scrollback ring, so a reattaching client gets a snapshot of the
current screen instead of a raw replay. The spike (a Go program driving `top`, `vim` and the real Claude
TUI in a PTY at 100x30) found:

- **Snapshots are lossless.** Replaying `Render()` into a fresh emulator gave 0 differing lines for top,
  vim (alternate screen) and Claude's trust prompt. Replaying the raw ring into a client of a different
  size did not match, which is the failure the emulator exists to avoid.
- **Resize works.** `pty.Setsize` plus `emu.Resize` plus the child's SIGWINCH made top and Claude
  re-wrap at 70x20.
- **Snapshot recipe.** `Render()` separates rows with a bare `\n`, which staircases in a real terminal,
  so a snapshot is: `ESC[?1049h` if `IsAltScreen()`, `ESC[H ESC[2J`, the render with `\n` replaced by
  `\r\n`, then the cursor position (`CursorPosition`) and `ESC[?25h/l` from `CursorHidden`.
- **Not captured:** terminal modes the app set (bracketed paste, mouse, kitty keyboard). The session host
  tracks those itself from the output stream and re-sends them in the prologue, or paste and mouse
  misbehave after a reattach. This needs a test.
- **Must drain `emu.Read`.** The emulator writes the app's query responses (device attributes, cursor
  position) to its `Read` side; they have to be copied back to the PTY, or the app waits for them.
- **Slow on bulk scrolling:** about 1 MB/s for 12 MB of scrolling output (with scrollback on or off).
  Claude's output is KB/s, so this is fine; a runaway `cat` must not stall clients, so the emulator is fed
  from its own goroutine while raw bytes go straight to attached clients.
- **Version risk.** The module is `v0.1.0` with an unreleased `ultraviolet` dependency. Pin it, and keep
  it behind a small interface (`Snapshot() []byte`, `Resize`, `Write`) so it can be replaced.
- The spike did not test a long real Claude session (tool output, status line churn) or Unicode edge
  cases. The first session-host task runs that soak test before relying on snapshots.

Why not zellij: one code path for web, SSH and runs instead of two; no nested multiplexer inside the
web terminal (its key bindings, mouse and status bar clash with Claude's); no extra dependency to
install on every host; and the registry knows exactly which processes are primary sessions. What zellij
gave for free, and we now write: multi-client attach, resize handling and scrollback replay (about
300 lines; run terminals already replay from the start).

**Ownership.** Runs as a dedicated agent user (`wrangl-agent`, own home, own `claude` login; created in
stage 2 below), not as the owner. That is the isolation from the owner's home and the controller's
data. It starts with the allowlist in section 6, not with bypassed prompts.

**Access.**
- **Web.** A "Session" button on each project row and project page opens the host's terminal view
  (`/runner/<host>/term`) on a tab named after the project, attached to the session host's PTY, shared
  live with every other client. Watch-only
  until "Take control", as with other tabs.
- **SSH.** `wrangl session ls` lists primary sessions with state and Remote Control name;
  `wrangl session attach <slug>` connects to the session host's socket in raw mode (resize forwarded, detach
  with a key chord that is not a Claude binding). Plain `ssh host -t wrangl session attach <slug>`
  works. `wrangl session restart <slug>` and `stop` exist too.

**Cost.** An idle session held 290-770 MB RSS in the sample above. Three active projects add about
2 GB. This is added to the host reserve in ADR 0009 (section on host capacity) so run placement
accounts for it.

## 4. Workers view (all Claude sessions)

**Rename.** Runners becomes **Workers** and includes the controller host. Routes: `/workers` (with
`/runners` redirecting), `/runner/<name>` stays. The nav label, page title, docs and CLI help change.

**Inventory.** Each runner daemon, the host's included, scans for Claude processes on every heartbeat
and reports one record per session:

`pid, kind, project, task/run, cwd, started, rss, cpu, remote-control name`

Kinds, decided in this order: **run** (the process, or an ancestor, has `WRANGL_RUN_ID`), **primary**
(a child of the session host), **shell** (started from a wrangl Shell tab),
**other** (everything else: a hand-started or Remote Control session).

`other` sessions are shown, flagged "unmanaged", with age and memory. They are never killed
automatically. The view offers "Adopt" (link to a project) and "Stop" with a confirm. The scan reads
`/proc` only (comm, ppid chain, cwd, `environ` where readable); it needs no ptrace and no hooks.

**UI.** Each worker card gets a "Sessions" list with those rows, each with a terminal link when a PTY
is attachable. A top-line count shows managed versus unmanaged sessions. EC2 runners report the same
way, so a stray `claude` on a runner is visible too.

## 5. Making wrangl the front door for work

Three layers, from soft to hard.

1. **`wrangl delegate "<title>" -f prompt.md [--queue] [--project P]`**: creates a task under a
   per-project "Delegated" feature and queues it in one step. `wrangl task wait SQID` blocks for the
   result (or a notification on completion). Today it takes three or four commands, which is why
   shortcuts win.
2. **`wrangl hook agent-guard`**: a `PreToolUse` hook, shipped in the binary so it is versioned with the
   controller. It reads the tool call from stdin and
   - denies `Agent` with a writing type (`general-purpose`, `claude`, `fork`) or
     `run_in_background`, and the `Workflow` tool, with a message that names `wrangl delegate`;
   - allows read-only types (`Explore`, `Plan`, `claude-code-guide`);
   - denies `Bash` that starts another Claude (`claude -p`, `claude --remote-control`, or `tmux`/`zellij`
     running `claude`), outside the session host, outside the supervisor.
3. **Host backstop.** The primary sessions and everything under the agent user run in a systemd slice
   with a CPU and memory share. A fan-out that slips through cannot starve runs.

Not covered: Remote Control or web sessions on other machines, and an owner who edits their own
settings. Managed settings (section 6) close the second.

## 6. Proposed permissions

Applied by `wrangl policy install`, which writes `/etc/claude-code/managed-settings.json` (needs root,
so a session cannot rewrite it) for the controller host, and a per-project `.claude/settings.json`
for the primary worktree. `wrangl policy show` prints what is in force. Nothing is installed until
the owner approves.

**A. Primary session (interactive, as `wrangl-agent`)**

| | Rule |
|---|---|
| allow | `Read`, `Glob`, `Grep`, `WebFetch`, `WebSearch`, `Artifact`, `Skill`, `Monitor`, `ToolSearch`, `AskUserQuestion` |
| allow | `Edit`, `Write` inside the primary worktree and the session scratch directory |
| allow | `Bash(git status\|diff\|log\|show\|fetch\|pull\|add\|commit\|checkout\|switch\|branch\|worktree\|stash\|restore\|rebase:*)` |
| allow | `Bash(gh pr:*)`, `Bash(gh issue:*)`, `Bash(gh run:*)`, `Bash(gh api:*)` (the hook rejects non-GET methods) |
| allow | `Bash(wrangl:*)`, except the confirm-list below |
| allow | `Bash(go build\|test\|vet\|fmt\|mod:*)`, `gofmt`, `task`, `mise`, `sed`, `grep`, `rg`, `cat`, `ls`, `find`, `head`, `tail`, `wc`, `sort`, `jq`, `diff`, `mkdir`, `sleep`, `timeout`, `python3 -I:*` |
| allow | `Bash(curl:*)` to `127.0.0.1`/`localhost` only (the hook checks the host) |
| allow | `Bash(aws ec2 describe-*)`, `Bash(aws ce get-*)`, `Bash(aws ssm describe-*\|get-*)` |
| allow | `Agent(Explore)`, `Agent(Plan)`, `Agent(claude-code-guide)` |
| ask | `Bash(wrangl task delete\|cancel:*)`, `wrangl project delete`, `wrangl pr approve\|close`, `wrangl fleet terminate`, `wrangl template delete`, `wrangl secret *` |
| ask | `Bash(git push:*)` (never to the default branch), `Bash(gh pr merge:*)`, `Bash(systemctl --user restart wrangl-controller)`, any `cf` command other than `list`/`get`, any `aws` write, `ssh` |
| deny | `Agent(general-purpose)`, `Agent(claude)`, `Agent(fork)`, `Workflow` (use `wrangl delegate`) |
| deny | `Bash(git push --force:*)`, `Bash(git push*main*)`, `Bash(git reset --hard origin/*)`, `Bash(rm -rf:*)` outside the worktree, `Bash(sudo:*)`, `Bash(chmod -R:*)` |
| deny | `Read`/`Edit`/`Write` on `~/.ssh/**`, `~/.aws/**`, `~/.config/wrangl/**`, `~/.local/share/wrangl/**`, `~/.claude/settings*.json`, `/etc/claude-code/**`, any `*.pem`, `*secret*`, `.env*` |

**B. Run agents on the host (queued, unattended, as `wrangl-agent`)**

They need fewer tools than the primary session, so they get a narrower set: `Read`, `Glob`, `Grep`,
`Edit` and `Write` in their worktree, the wrangl MCP tools, and `Bash` for `git`
(status/diff/log/add/commit/checkout/stash), `go`, `gofmt`, `task`, `mise`, `sed`, `grep`, `cat`, `ls`,
`python3 -I`, `sleep`, `mkdir`. No `gh`, `aws`, `cf`, `ssh`, `curl` to anything, `systemctl`, `sudo`.
Push is done by the daemon with its own token, never by the agent. Prompts stay `acceptEdits` plus this
allowlist; `bypassPermissions` is dropped on the host once the agent user exists. On EC2 runners it
stays as it is (disposable machines with only the run's own token).

**C. Hooks (both)**: `agent-guard` as in section 5, and a `PreToolUse` `Bash` hook that rejects
commands writing outside the worktree by path argument (a cheap check, not a sandbox: the agent user
and filesystem permissions are the real boundary).

## 7. Staging

1. **Sessions inventory and Workers.** The scan and heartbeat field, the rename and redirect, the
   Sessions list on cards.
2. **Agent user on the host.** `wrangl-agent` user, its home and `claude` login, the runner's existing
   drop mode enabled for the host, the slice limits, the ADR 0009 reserve updated.
3. **Primary sessions.** The session host service (PTYs, ring, socket), the controller reconciler, naming,
   resume, config, `wrangl session ls|attach|restart|stop`, health line on the project page.
4. **One-click access.** The Session button and the attach-tab, the terminal view naming.
5. **Policy.** `agent-guard`, `delegate`, `task wait`, `policy install|show`, the permission files from
   section 6, docs and the wrangl skill rule.

## 8. Open questions (recommendation first)

1. **SSH as which user?** Recommend a group-owned session-host socket (`wrangl-sessions` group, mode 0660), so the owner
   attaches without `sudo`. The alternative is `sudo -u wrangl-agent`.
2. **Primary session permission mode.** Recommend the section 6 allowlist with prompts on, not bypass.
   You will see more prompts than today; the `ask` list is deliberately short.
3. **Which projects get one.** Recommend every non-archived project with a linked repo; projects without
   a repo get none until one is linked.
4. **Managed settings versus user settings.** Recommend managed (root-owned) so sessions cannot loosen
   their own policy. It needs a one-time `sudo wrangl policy install` from you.
5. **Second Claude login for the agent user.** Recommend yes if the account allows it, so run agents
   and your sessions do not share one usage limit.
6. **`other` sessions.** Recommend show only, never auto-stop.

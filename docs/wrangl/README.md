# wrangl documentation

wrangl is a self-hosted controller that runs a ranked backlog of tasks on a fleet of coding-agent
runners and hands the questions and pull requests back to you. The top-level
[README](../../README.md) is the overview; this page is the map.

## Start here

1. **Install.** [INSTALL.md](INSTALL.md) takes an empty AWS account to a working controller, one GitHub
   App per account, a Claude token, a first template and a first runner. AWS roles and policies:
   [aws-iam.md](aws-iam.md).
2. **First project.** `wrangl project create web --template go-claude`, then
   `wrangl repo link web your-org/your-repo`. What each setting does: [guide/projects.md](guide/projects.md).
3. **First task.** `wrangl task create web --title "..." --prompt "..."`, optionally
   `wrangl task split SQID --title A --title B --chain`, then `wrangl task queue SQID`. Watch it with
   `wrangl run list --state active` and `wrangl run attach RUN`. See [guide/tasks.md](guide/tasks.md),
   which ends with a worked example from creation to merge.
4. **Read the Inbox.** Everything that waits on you (agent questions, the "What now?" card, plan
   proposals, task reviews, PR reviews, deliverables, tool failures) is in the Inbox and on the
   project page's "Needs you" strip; the CLI equivalent is `wrangl question list` and
   `wrangl question answer ID OPTION`. Kinds and answers: [tasks guide, section 6](guide/tasks.md#6-inbox-questions-and-reviews).
5. **If something waits.** [guide/scheduling.md](guide/scheduling.md#8-why-is-my-run-waiting).

## The guides

| Guide | Read it to learn |
|---|---|
| [Projects](guide/projects.md) | project settings, the project page and chat, runner limits and the effective cap, archiving, GitHub repos and issues |
| [Tasks](guide/tasks.md) | the task tree, types, states, needs, runs, checkpoints, the Inbox, PRs, Smart Merge, deliverables |
| [Runners](guide/runners.md) | templates, enrolment, sessions and worktrees, self-upgrade, cordon/drain/terminate, remote targets, the host runner |
| [Task scheduling](guide/scheduling.md) | how a queued run becomes a running session, the autoscaler, and the known limits |

## Reference

- **Design and development.** [DESIGN.md](DESIGN.md) (architecture and what each feature does),
  [DEVELOPMENT.md](DEVELOPMENT.md) (build from source, web UI conventions, releasing) and the
  [release notes](../releases/).
- **MCP.** [mcp.md](mcp.md): the owner's MCP endpoint, how to connect it, and every tool.
- **CLI.** `wrangl --help` and `wrangl COMMAND --help` are authoritative. The agent skill that maps the
  commands for Claude is [skill/SKILL.md](skill/SKILL.md).
- **Agents.** [agents.md](agents.md): how each agent's hooks, sessions, MCP and auth plug in, and the
  GitHub issue tools.
- **Remote targets.** [android.md](android.md) (emulator host) and [ADR 0002](adr/0002-remote-runners.md).
- **Templates.** Examples in [templates/](templates/):
  [claude-headless](templates/claude-headless.yaml) (the default),
  [claude-interactive](templates/claude-interactive.yaml), [codex](templates/codex.yaml),
  [gemini](templates/gemini.yaml), [antigravity](templates/antigravity.yaml),
  [design](templates/design.yaml), [research](templates/research.yaml),
  [host](templates/host.yaml), [remote-mac](templates/remote-mac.yaml),
  [remote-android](templates/remote-android.yaml).
- **Deploy.** `deploy/wrangl/controller.sh up|upgrade|status|down` (see INSTALL.md sections 3 to 5 and 12).
- **Design.** [original design spec](../superpowers/specs/2026-10-31-wrangl-design.md), UI mockups in
  [docs/design/](../design/), logo and diagram sources in [docs/brand/](../brand/README.md).

## Architecture decision records

| ADR | Status |
|---|---|
| [0001 Agent definitions vs templates](adr/0001-agent-definitions-vs-templates.md) | accepted, built |
| [0002 Remote runners](adr/0002-remote-runners.md) | accepted, built (experimental) |
| [0003 Per-project custom tools](adr/0003-project-custom-tools.md) | accepted, **not built** |
| [0004 Agent GitHub issue tools](adr/0004-agent-github-issue-tools.md) | accepted, built |
| [0005 Projects as the primary lens](adr/0005-projects-as-primary-lens.md) | accepted, built |
| [0006 Action state machine](adr/0006-action-state-machine.md) | accepted, **not built** |
| [0007 Capabilities vs capacity](adr/0007-capabilities-and-capacity.md) | accepted, **not built**; slots/pools/caps superseded by 0009 |
| [0009 Resource-based placement, cost budget, unplaceable item](adr/0009-resource-based-placement.md) | accepted 2026-11-09, **being built** (stage by stage) |
| [0008 Runners belong to the system](adr/0008-runners-belong-to-the-system.md) | accepted, **not built** (was a second "0005") |

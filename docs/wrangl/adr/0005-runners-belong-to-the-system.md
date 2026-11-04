# ADR 0005: Runners belong to the system, not to a project

- Status: **Proposed** (the owner decides before any implementation starts)
- Task: JZ1IiM
- Builds on: [ADR 0002](0002-remote-runners.md); migrations 00020 (`runner_template.max_sessions`) and 00025 (`project_template_limit`, `runner.launched_for_project`)
- Touches (future work): `controller/scheduler` (`scheduler.go`, `assign.go`), `db/queries/run.sql` and `runner.sql`, `controller/projects`, `api`, web runners/project pages, `wrangl project edit`, `SKILL.md`, `INSTALL.md`, a DB migration, `runner/work.go` (secret files)

This ADR is design only; no application code changes with it.

## Context

A runner already runs several sessions at once (`max_sessions`, default 4). `ClaimIdleRunner` and
`FreeRunnerSlot` pick a runner by **template** only (not by project) and pack busy runners first. So
the *placement* half of the goal mostly exists. The project coupling that remains is elsewhere:

1. **Provisioning is per project and template.** `ProjectTemplateDemand` groups queued runs by
   (project, template); `PlanLaunches` splits the unmet demand among projects in priority order, each
   rounding up to whole hosts. Two projects each wanting 2 sessions can launch two runners where one
   would do.
2. **Limits are per (project, template) and count runners.** `project_template_limit.max_instances`
   (default 1 for the project's default template, 0 for the rest) is compared with
   `runner.launched_for_project`, a *launch-time* attribution. It is not a usage measure: a runner that
   project A launched and project B is now using counts only against A, and a runner that has moved on
   to other work still counts against A until it is terminated.
3. **Runs carry a template.** `COALESCE(run.template_id, task.template_id, project.default_template_id)`
   chooses the runner pool. That is a real capability requirement (OS, size, toolchain) and stays.
   What is wrong is that the *project's* default and limits decide which pool it may use.
4. **The UI/API describe a runner as belonging to a project** (`launched_for_project`).

### Isolation as it is today (audit)

Sessions on one runner already run as one OS user (`agentIdentity`, one `$HOME`). Findings:

| Surface | Today | Verdict for cross-project sharing |
|---|---|---|
| Workdir | `$HOME/work/<runID>`; run ids are global | Safe: unique per run. |
| Repo storage | one bare mirror per **clone URL**, one worktree per run, git writes under a per-mirror lock | Safe: a mirror is only shared by sessions of the same repo, and a repo belongs to one project. A repo linked to two projects shares only its public objects. |
| Git credentials | `Assignment.GitToken` is a one-repo, per-assignment installation token, passed to that session | Safe provided the token is never written to shared git config. **Verify with a test.** |
| Agent env | built per assignment (`Assignment.Env`, secrets resolved by name on the controller) | Safe: the process env is per session. |
| Secret **files** | `SecretFile.Path` is written to a path under the shared `$HOME`, ref-counted across sessions (`secretRefs`, `dropSecret`) and removed with the last holder | **Leak risk.** A file written for project A's agent is readable by project B's agent on the same runner, and with the same `Path` but different content from another project the ref-count would keep the first content. |
| Session names | `<runner>:<task sqid>` | Safe: task sqids are global; but a name is shown to anyone on the runner page, so it should not be treated as private. |
| Same OS user | all sessions can read each other's workdirs | Cross-session reads are possible by design. Mitigation below. |

## Options

### Unit of the per-project cap

- **A. Sessions (chosen).** A project may occupy at most N sessions at once (counting runs in
  `assigned..cancelling` states). Fits the 4-slot model: a runner is just 4 slots. The UI still shows
  the number of runners a project is currently on, derived from its sessions.
- **B. Runners.** Keep "N runners". A runner is shared, so a project "has" a runner when it has at
  least one session on it; a cap of 1 runner would then mean 1 to 4 sessions, which is not a
  predictable budget, and packing would fight the cap.

### Placement and provisioning

- **1. Pooled by template, project-blind (chosen).** Placement picks any compatible runner with a
  free slot. Demand is `sum(queued) - free slots`, runners to launch = `ceil(need / max_sessions)`,
  bounded by the template's `max_runners`. Fairness is by project priority only when slots are scarce.
- **2. Soft affinity.** Prefer a runner already serving the same project, to improve mirror/cache
  reuse. A cheap tie-breaker after "busy first"; it must never block placement. Left as a later,
  optional refinement (not part of this change) so the first version stays simple.
- **3. Reserved capacity per project.** Keep runners dedicated to a project. Strongest isolation, but it
  is exactly the waste this task removes and needs a second knob. Rejected.

### Isolation approach

- **I. Per-session subdirectory and file scoping (chosen).** Keep one OS user; put secret files under
  the session's own dir (`$HOME/work/<run>/.wrangl-secrets/`, mode 0600, removed with the session);
  drop the cross-session ref-count.
- **II. One OS user per session.** Strong isolation (separate uid, home, mirror access via group).
  Much bigger change in the runner applier and mirror locking; defer. Revisit if an owner runs
  mutually untrusted projects on one fleet. Until then the docs state that **projects sharing a
  runner share its trust boundary**; an owner who needs hard separation uses a different template
  (a different pool) per project, which still works because a task or project may choose a template.

## Decision

1. **Scheduling.** Placement stays template-compatible and project-blind; `ClaimIdleRunner` and
   `FreeRunnerSlot` do not change in meaning (a test pins it with two projects). Queue order stays
   `project.priority DESC, queue_rank, ...`.
2. **Cap unit: sessions.** New `project.max_sessions` (NULL = no cap beyond the fleet). The scheduler
   skips a queued run when its project's active sessions are at the cap and moves on to the next
   project in priority order, so a capped project does not block others. Active sessions are counted
   from `run` states, not from `launched_for_project`, so the cap measures usage.
3. **Migration path.** Add `project.max_sessions`. Backfill it with
   `SUM(project_template_limit.max_instances * runner_template.max_sessions)` per project, with a floor
   of the default template's `max_sessions` (preserving today's "1 runner of the default template").
   Keep `project_template_limit` and `wrangl project edit --limit TEMPLATE=N` for one release: the flag
   is accepted, prints a deprecation notice, and translates to the session cap (`N * max_sessions`).
   Then drop the table and `runner.launched_for_project` (it is kept meanwhile as a hint for the
   audit trail, not for limits). `project.default_template_id` remains: it is the default
   *requirement* for runs, not an ownership link.
4. **Provisioning demand is global per template.** `TemplateDemand` already is; replace the per-project
   `PlanLaunches` split with one computation: `need = capped_demand - supply`,
   `launch = min(ceil(need / max_sessions), max_runners - live)`. `capped_demand` counts, per project,
   `min(queued, cap - active)`, so a capped project does not provoke runners it cannot use. Launches
   no longer record a project; manual launch (`runner launch`) loses its per-project check and keeps
   the template ceiling.
5. **Isolation.** Move secret files to a per-session directory (Option I), add tests listed below, and
   document the shared trust boundary.
6. **API/UI.** Runner objects get `sessions: [{run, task, project, state}]` and `slots: {used, max}`.
   Projects get `runners: [{name, template, sessions_for_project, also_serving: [project...]}]`.
   The runners page shows "n of 4" with project tags; the project page shows "runners N active" and
   "also serving X" on shared ones.
7. **CLI/docs.** `wrangl project edit --max-sessions N`; `runner list` shows slot use and projects;
   `SKILL.md` and `INSTALL.md` describe pooled runners, the cap, the trust boundary, and the deprecated flag.
8. **Draining/upgrading.** A runner with `drain_requested` or `upgrade_sha` is skipped for new claims
   (already so) and keeps its sessions of all projects until they finish. The drain/upgrade finisher
   must wait for sessions regardless of project; a test covers it.

## Rollout: small PRs

1. This ADR.
2. Tests first (no behaviour change): scheduler with two projects on one runner; isolation tests.
3. Migration + `project.max_sessions` + cap enforcement in the scheduler; fairness test.
4. Provisioning: global demand, drop the per-project `PlanLaunches` split and manual-launch project check.
5. Secret files per session in the runner.
6. API + UI (runners page, project page).
7. CLI, `SKILL.md`, `INSTALL.md`; deprecate `--limit`; later PR drops the old table and column.

## Consequences

- Fewer instances for the same load (ceil over all projects instead of per project), so lower cost.
- A project can no longer be pinned to "its" runner; a noisy project is bounded by its session cap
  and by priority ordering, not by a private pool.
- The default for a project with no cap is "unbounded within the fleet", and the template's
  `max_runners` is the real ceiling. Owners who relied on "0 instances of an expensive template" lose
  that guard: the migration therefore keeps the guard by restricting which templates a project may
  *request* (a follow-up if needed; flagged for the owner's decision).
- Trust boundary: projects on shared runners are not isolated from each other at the OS level.
- Remote-target templates keep per-target slots; their driver sessions follow the same cap and
  pooled rules (driver demand is added to the driver template's global demand).
- Risks: the backfill rounds limits to session multiples; priority-based fairness can starve a
  low-priority project under sustained load (acceptable, existing behaviour).

## Open questions for the owner

1. Is "projects share a trust boundary" acceptable, or is per-session OS-user isolation (Option II) required first?
2. Should a project with no stored limits get a cap of `max_sessions` of the default template (the conservative backfill above) or no cap?
3. Should soft project affinity (Option 2) be included in the first release?

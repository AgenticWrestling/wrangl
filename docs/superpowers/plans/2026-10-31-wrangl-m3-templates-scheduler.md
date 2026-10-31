# wrangl — M3: Agent User, Template Apply, Scheduler Scale-up, Cordon/Drain

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Spec:** `docs/superpowers/specs/2026-10-31-wrangl-design.md` (§6, §8 "Runner users", D26)

**Goal:** Runners install their template's toolchain for an unprivileged `agent` user, re-apply
it when the template is edited, and report the result. The controller launches capacity when
runs are queued, and cordon/drain work. Running tasks is M4: a queued run here only drives
scale-up.

## Decisions

1. **Users (D26).** The daemon runs as root and owns `/etc/wrangl/*` (creds 0600). Bootstrap
   creates the `agent` user, with no sudo. The `wrangl` user and the poweroff sudoers rule are
   removed.
2. **Template reconciliation is driven by heartbeats.** Each heartbeat carries the runner's
   applied `template_version` and whether an apply is in progress. When the applied version is
   behind the template's version, nothing is applying, the template's current version hasn't
   failed, and the runner is `enrolling`/`idle`/`provisioning`, the controller sets
   `provisioning` and sends `cmd.template`. A runner already `provisioning` is only re-sent
   after 60 s, which covers a daemon restart mid-apply. Edits reach runners that were offline
   at the time when they come back.
3. **Apply on the runner** runs each step only when needed, so re-applying is safe:
   1. install mise to `/usr/local/bin` if it's missing
   2. run `setup_script` as root when its sha256 changed
   3. write `~agent/.config/mise/config.toml`
   4. run `mise install --yes` as `agent`

   Progress lines and a final result (ok or error, plus the last 8 KiB of output) are published
   to JetStream on `wr.evt.<r>.template`. The applied version and the setup hash are kept in
   `/var/lib/wrangl/`.
4. **Failure.** The controller records `runner.last_error` and `template_log`, sets
   `template.failed_version = version` and cordons the runner. A cordoned idle runner is still
   reaped after the idle timeout, so a broken box doesn't run up cost. The scheduler won't
   launch for a template whose current version failed; `template edit` bumps the version and
   clears the failure.
5. **Cordon and drain are flags, not states.** `runner.cordoned` and `runner.drain_requested`
   sit alongside the activity state (`idle`, `busy`, …). Cordoned runners get no new work, but
   the idle reaper still applies. Drained runners are terminated as soon as they're idle;
   adopted runners are cordoned instead, since the controller can't terminate them.
6. **Scale-up.** Every 10 s the scheduler works out, for each template:
   - demand = queued runs whose task's effective template is this one (the task's own
     template, else the project default; full inheritance comes in M4)
   - supply = runners on that template that are uncordoned and `launching`, `enrolling`,
     `provisioning` or `idle`

   It launches `min(demand − supply, max_runners − live runners)`. After a launch error a
   template backs off, doubling from 1 min to a 30 min maximum, so capacity or IAM errors don't
   hammer EC2.

## As built (2026-10-31)

M3 is complete. Findings and deviations:

- **No default mise installer.** The first e2e run after the daemon learned `cmd.template` made
  the runner run the real installer (`curl https://mise.run | sh`) from a test. The proxy here
  blocks `mise.run` (403), so nothing was fetched or executed, but the cause was real. Now an
  `Applier` with no installer refuses to install, and only `wrangl runner run` wires in
  `DefaultMiseInstaller`. That installer also saves the script to a file before running it,
  because with a pipe a failed download gives `sh` an empty script that "succeeds".
- **Shutdown ordering.** Under `-race` the e2e test showed scheduler and reaper ticks running
  after `Run` had closed the database. `Run` now cancels the loops **and waits for them** before
  the NATS and DB teardown.
- **Real mise is untested.** Running the downloaded installer is not permitted in this session,
  so every apply was exercised against `runnertest.FakeMise`. The first real runner should be
  watched for mise download/install behaviour, agent PATH/shims, and tool versions.
- **Manual run, real binaries.** The whole flow was run end to end: enrol, setup script as the
  daemon, agent mise config, v1 applied → idle. Then edit → v2 applied, broken edit → v3 failed
  with mise's error, cordoned, log tail shown, and cordon/uncordon/drain. Edits land on the
  next heartbeat, so allow up to 10 s.

## Tasks

- [x] T1 Migration `00003`: `runner.cordoned`, `runner.drain_requested`, `runner.last_error`,
  `runner.template_log`, `runner.provisioning_since`, `runner_template.failed_version`.
- [x] T2 proto: `Heartbeat.TemplateVersion`, `Heartbeat.Applying`; `TemplateApply` command;
  `TemplateProgress` / `TemplateResult` events.
- [x] T3 Runner: config gains `state_dir`, `agent_user`, `mise_bin`; bootstrap creates `agent`
  and drops the `wrangl` user, sudoers rule and `User=`; `apply.go` (steps above; commands run
  as `agent` with dropped credentials when root, or as the current user in tests); the daemon
  handles `cmd.template` (single flight) and reports version and applying in heartbeats.
- [x] T4 Controller fleet: reconcile on heartbeat; a JetStream durable consumer for template
  results; `UpdateTemplate` (version bump, failure cleared); cordon/uncordon/drain; the drain
  sweep; the reaper also covers cordoned idle runners.
- [x] T5 Scheduler package: scale-up with backoff; a loop in serve.
- [x] T6 API/client/CLI: `PUT /api/v1/templates/{name}`, `template edit [-f] (else $EDITOR)`,
  `POST /api/v1/runners/{name}/{cordon,uncordon,drain}`, and the template status, error and log
  tail in `fleet show`.
- [x] T7 Tests: apply against a fake `mise` on `PATH`; reconciliation, failure and edit; the
  scheduler with the fake EC2; e2e extended so a runner applies its template, goes idle, picks up
  an edit, and a queued run triggers a launch.
- [x] T8 Docs + as-built.

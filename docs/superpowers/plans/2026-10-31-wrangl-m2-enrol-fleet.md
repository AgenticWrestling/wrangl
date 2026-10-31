# wrangl — M2: NATS Security, Enrolment, Runner Daemon, Fleet Lifecycle

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Spec:** `docs/superpowers/specs/2026-10-31-wrangl-design.md` (rev 2, §6–§7, §13)

**Goal:** Runners enrol with a one-time token, get NATS credentials they generated the key for,
connect over TLS with per-runner subject permissions, and heartbeat. The controller tracks them,
marks them offline, and **terminates idle runners after 30 minutes**. Every EC2 resource the
controller creates is **tagged**, so `wrangl fleet teardown` can find and remove all of it even
when the controller itself is gone.

**Scope pulled forward from M3** (owner request, 2026-10-31): EC2 tagging, `fleet launch`,
`fleet terminate`, `fleet teardown`, the idle reaper, and `template create/list/show/delete`
(launch needs a template's minimum specs). Still in M3: the scheduler (queue-driven scale-up),
template *apply* on runners, cordon/drain.

**Tech Stack additions:** `nats-io/jwt/v2`, `nats-io/nkeys`, `shirou/gopsutil/v4`,
`aws-sdk-go-v2/service/{ec2,ssm}`.

## Decisions taken in this plan

1. **Tags.** Every instance, root volume and network interface is tagged:

   | Tag | Value |
   |---|---|
   | `Name` | `wrangl-<runner>` |
   | `wrangl:managed` | `true` |
   | `wrangl:controller` | the controller id (random, made at `init`, stored in `meta`) |
   | `wrangl:runner` | runner name |
   | `wrangl:template` | template name |

   The controller **only ever terminates instances carrying `wrangl:managed=true` and its own
   `wrangl:controller` id**, and it checks the tags on the live instance immediately before every
   terminate call.

2. **Idle shutdown.** `runner_template.idle_timeout_s` defaults to 1800. The reaper runs every
   minute and terminates controller-launched runners in these cases:
   - `idle` for longer than the idle timeout (`idle_since` is set whenever a runner becomes idle)
   - `offline` (no heartbeat) for longer than the idle timeout
   - still `launching` or `enrolling` 15 minutes after launch (enrolment failed)

   Adopted runners (enrolled through `fleet enroll-token`, not launched by the controller) are
   never terminated. They're logged and left alone.

3. **Safety nets for orphans.** These matter if the controller dies, because then nothing reaps.
   - Instances launch with `InstanceInitiatedShutdownBehavior=terminate`.
   - The runner daemon powers the box off after **2 h without the controller**, which therefore
     terminates it.
   - `wrangl fleet teardown` calls AWS **directly** (local credentials, `--region`,
     `--profile`). It does not go through the controller, lists every tagged instance, and
     terminates them after confirmation.

4. **Enrolment proof.** The spec said to verify the EC2 identity document signature, which needs
   embedded per-region AWS certificates. M2 instead requires, for controller-launched runners:
   the token is bound to the instance id returned by `RunInstances`, **and** the enrol request's
   source IP equals that instance's private IP (`DescribeInstances`). Both sides are inside the
   VPC with no NAT, so this proves the request came from the instance. Adopted runners are token
   only.

5. **NATS TLS.** `init` creates a CA, stored under `nats/`. `serve` issues a fresh server leaf
   certificate at every start, with SANs for the configured NATS host plus 127.0.0.1 and
   localhost, so changing the listen address never leaves a stale certificate. Runners pin the
   CA they receive at enrolment.

## As built (2026-10-31)

M2 is complete. Every task below is done. Findings and deviations:

- **NATS TLS and the in-process connection.** TLS applies to `nats.InProcessServer` too.
  Without `nats.Secure` on the controller's in-process connection, the handshake fails and
  nats.go deadlocks while closing the pipe. The test hung until this was found.
- **Runner inbox.** A runner must connect with `nats.CustomInboxPrefix("_INBOX_<id>")`.
  Otherwise even JetStream publish acks are refused by its permissions. `runner.Connect` sets
  it, and the serve test asserts it.
- **sqlc quirk 5.** Mixing bare `?` with `sqlc.narg()` in one query shifted the arguments
  (`missing argument with index 10`). Every query is now checked so the two styles never mix.
  `ListRunners` uses `sqlc.embed(r)` to get a nested struct across the join.
- **Idle clock across blips.** The e2e test showed that an offline-to-idle transition reset
  `idle_since`, so a network blip restarted the 30-minute clock. `RecordHeartbeat` now keeps
  it (`COALESCE`).
- **AWS not exercised for real.** The session's AWS keys are invalid and the metadata endpoint
  is blocked, so the EC2 code is tested only against `provision.Fake`. The first real
  `fleet launch` should be watched: AMI parameter resolution, `RunInstances` with
  `TagSpecifications` for volume and network-interface, and the IAM policy in
  `docs/wrangl/aws-iam.md` are all unverified against AWS.
- **Manual run.** With the real binaries: init → serve → template create → enroll-token →
  runner bootstrap → runner run → `fleet list` (live metrics) → `runner status`. Launch without
  AWS returns 503, and terminating an adopted runner returns 403.

## Tasks

- [x] **T1 Schema `00002`:** `runner.idle_since`, `runner.launched` (bool),
  `runner.launched_at`, `runner.enrolled_at`. Queries: enrol, heartbeat, mark offline, reaper
  candidates, token mint/consume. `meta.controller_id` set at `init`.
- [x] **T2 `natsd`:** operator, `SYS` and `WRANGL` account (with a signing key) generated at
  `init`. Seeds go in `nats/*.nk` (0600). CA + leaf certificates. Server options in operator mode
  with a memory resolver and JetStream limits on the account. Controller user JWT (full access).
  `IssueRunner(runnerID, pubkey)` returns a user JWT with the §7 permissions. **Tests:**
  - a runner can publish its own events into JetStream
  - a runner cannot subscribe to another runner's `cmd` or publish another runner's events
  - a runner can reply to controller requests
  - TLS is required on TCP, and the in-process controller connection still works
- [x] **T3 `proto`:** subjects, `Heartbeat`, `EnrollRequest` and `EnrollResponse`.
- [x] **T4 Enrol listener (`:4223`):**
  - `POST /enroll` checks: token hash, unused, unexpired, instance binding and source IP for
    launched runners, and that facts meet the template minimums. It burns the token, sets
    `nkey_public` and returns `{runner_id, name, user_jwt, nats_urls, nats_ca}`.
  - `GET /dl/wrangl-linux-{arch}` serves `data/bin/…`, falling back to the controller's own
    executable when the arch matches.
  - **Tests:** reused, expired and wrong-instance tokens, IP mismatch, specs below the minimum,
    a valid enrolment.
- [x] **T5 `provision`:**
  - `EC2API` interface plus a fake for tests.
  - `Launch(template, runner, userData)`: resolves the AMI (SSM parameter or AMI id), picks the
    instance type (allowlist, else `DescribeInstanceTypes` filtered by arch and the minimums,
    smallest first), sizes the root gp3 volume, requires IMDSv2, sets
    shutdown-behaviour=terminate and the tags.
  - `Terminate(id)` re-checks the tags first. `ListManaged(controllerID|all)` lists tagged instances.
  - User-data renders the binary download, sha256 check and bootstrap.
  - **Tests** against the fake: tags present on every resource type, terminate refuses untagged
    or foreign-controller instances, instance-type selection.
- [x] **T6 Controller loops:**
  - heartbeat ingest: `wr.r.*.hb` updates `last_heartbeat_at` and metrics, and marks the runner
    `idle` on its first heartbeat
  - an offline sweep: 60 s without a heartbeat means `offline`
  - the reaper, with an injectable clock
  - **Tests** with a fake clock and the fake EC2.
- [x] **T7 API + client:**
  - `GET /api/v1/runners`, `GET /api/v1/runners/{name}`, `POST /api/v1/enroll-tokens`,
    `POST /api/v1/runners` (launch), `DELETE /api/v1/runners/{name}` (terminate), plus
    `/api/v1/templates` CRUD.
  - A client over `unix://`, `http(s)://` with optional CF service-token headers.
  - CLI: `fleet list/show/enroll-token/launch/terminate/teardown`, `template create/list/show/delete`.
- [x] **T8 Runner:**
  - `bootstrap`: user `wrangl`, NKey, IMDSv2 instance id, enrol, write
    `/etc/wrangl/runner.{yaml,creds,ca.pem}`, systemd unit, enable. Every path and the IMDS URL
    can be overridden for tests.
  - `run`: TLS + creds, custom inbox, heartbeat every 10 s with gopsutil metrics, answers
    `cmd.ping`, and powers off after 2 h without the controller.
  - `status`: prints the local config and does a NATS round trip.
- [x] **T9 End-to-end test:** an in-process controller; the CLI mints a token; `runner
  bootstrap` against a fake IMDS with systemd disabled; `runner run`; `fleet list` shows the
  runner `idle`; advancing the fake clock 31 minutes terminates it through the fake EC2, and the
  tags were checked.
- [x] **T10 Docs:** spec §6, §7, §13 and §15 updated; README.

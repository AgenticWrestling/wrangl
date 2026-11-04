# ADR 0006: Action state machine (processing, failed, superseded, timed out) for every user action

- Status: **Proposed** (the owner decides before the implementation PRs start)
- Task: t8AZpr. Depends on the Projects lens (gmy0K3, merged) and the Datastar cleanup (LhOrqs) for the templates it touches.
- No application code is changed by this note.
- Builds on: `web.go` `act` and the `actions` table, `icard` in `inbox.html` (`data-indicator`, `.card.busy`, `.spin`), the live SSE patch (`/ui/live`, kinds per view), `prs.Service.merge`, `fleet.Service`, `runs.Cancel`.

## Context

Most owner actions are not instant, but the UI only knows two moods: "request in flight" and "done". What really happens, verified in code:

- `POST /ui/act/{name}` runs the service call **inside the HTTP request** (`act` calls `fn(h, a)` synchronously). `pr approve` is: optional GitHub review, GitHub merge, then `markMerged` (DB transaction, task recompute, `continueBlocked`). The response is a toast, plus `document.getElementById(..).remove()` for the card.
- The only "processing" state is client memory: `data-indicator:b<key>` sets a signal while the request is open and `data-class:busy` dims the card. Reload the page, open a second device, or lose the SSE connection and the card looks idle and clickable again.
- Failure is a toast (`toast(w, err.Error(), true)`) that disappears. The card stays as it was, with no reason and no next step. Merge refused because of conflicts, checks or a ruleset looks the same as a transient 502.
- "Stale" is one special case: `errs.Conflict` plus `res.dropEl` removes the card. Anywhere else a second click produces a raw error like `Conflict: question 16 is answered`.
- Several actions hand work off and finish *later*: Smart Merge queues a run; request-changes queues a revise run; Launch creates an EC2 instance that registers minutes later; Drain waits for idle; run Cancel moves a run to `cancelling` until the runner acknowledges. The click returns "ok" while the real outcome arrives as an entity change over SSE, or never.

So the owner cannot tell "working", "failed", "already done by someone else" or "stuck", and the task still looks open.

### Constraints

1. One small pure function, table-tested, used by every action (Go).
2. Pending state must survive reload and show on a second device: derive it from stored entity state, not only client memory.
3. Prefer Datastar signals, `data-indicator`, `data-attr`, `data-show` over custom JS (LhOrqs). Keep `.card.busy` and `.spin`.
4. Inline confirms (`data-confirm` sheet), never `confirm()`.
5. Few reusable patterns, not 40 bespoke handlers.

## The state machine (common to all options)

States: `idle`, `processing`, `succeeded`, `failed{reason, next[]}`, `superseded`, `timed_out`.

Events: `click`, `ack` (server accepted), `error{kind}`, `observed{entity}` (an SSE patch or a reload shows a stored state that proves the outcome), `tick` (deadline passed), `reload` (page load with stored state), `cancel`, `retry`, `dismiss`.

```
              click                ack (still pending)
   idle ───────────────▶ processing ───────────────┐
    ▲                     │  │  │                  │ (stays processing; deadline set from started_at)
    │ dismiss/retry       │  │  └─ observed(proof) ▶ succeeded ─▶ (card leaves / row flashes)
    │                     │  └──── error{refused}─▶ failed{reason,next}
    ├─────────────────────┤        error{stale}  ─▶ superseded ─▶ (card leaves, "already merged by …")
    │                     └─ tick(deadline) ─────▶ timed_out ─ observed(proof) ─▶ succeeded
    └── retry ◀── failed / timed_out ──────────────  timed_out ─ cancel ─▶ idle (or processing(cancel))
```

Rules that make it small:
- `observed` beats everything: an entity state that proves the outcome moves any non-terminal state to `succeeded` (or `superseded` when someone else did it, or `failed` when the entity shows the refusal, e.g. `pull_request.mergeable = conflicted`). This is what makes two sessions and reloads agree.
- `timed_out` is not failure. It means "no answer yet"; a later `observed` still wins. Offers: *Check again* (re-reads the entity, no side effect), *Cancel* where the kind allows it.
- `error{kind}` is classified once, server side, from `errs.*` and `gh.HTTPError`: `stale` (Conflict/NotFound on a resolved item) -> `superseded`; `refused` (Invalid, merge 405/409/422, conflicts, checks) -> `failed` with a reason code; `transient` (5xx, network, lock) -> `failed` with Retry; `auth` (GitHub App missing) -> `failed` with the existing fix link.
- `retry` is allowed only if the kind says so (idempotent or guarded). Non-retryable kinds show *Check again* instead.

Signature (package `internal/actions`, no deps, no I/O):

```go
type State uint8 // Idle, Processing, Succeeded, Failed, Superseded, TimedOut
type Event struct{ Kind EventKind; Err ErrClass; Observed Observation; Now time.Time }
type Status struct {
    State  State
    Reason string      // stable code, e.g. "conflicted", "checks", "gone"
    Since  time.Time   // when processing started (from storage, not the client)
}
func Next(k Kind, cur Status, ev Event) Status   // pure, total, table-tested
func Derive(k Kind, e Entity, now time.Time) Status // reload / second device: entity -> Status
```

`Kind` is a table row (one struct per action kind) holding: `Confirm(entity) Observation` (the proof: which entity state means success, which means superseded, which means a known failure), `Timeout`, `Retry bool`, `Cancel *CancelSpec`, and the copy (`Doing`, `Done`, `Reasons map[code]Copy`, `Next map[code][]Action`). Rendering takes `Status` and `Kind` and nothing else.

Per-kind data lives in one Go table, so the audit below is the table and the table is the audit.

## Options considered

### A. Client-only state (extend today's `data-indicator`)
Keep the synchronous POST; add a per-action signal `state` set by the response (`ok`/`err`/`stale`) and a client timer for timeout.
- Good: smallest diff; no schema change.
- Bad: fails constraint 2 (reload and second device lose it); the timeout and "proof" logic would be JS (against LhOrqs); every template re-implements it; no way to know a Smart Merge run or a launch finished except by the page happening to re-render.

### B. Persisted `action` table (generic ledger of requests)
New table `action(id, kind, subject, state, reason, request_id, created_at, finished_at, ...)`; every act inserts a row, services update it; UI renders from it.
- Good: uniform; any action gets history, retry, audit log, and works across devices; async work has a natural home.
- Bad: duplicates truth already held by entities (`pull_request.state`, `run.state`, `question.state`) and creates two sources that can disagree (the row says processing while the PR says merged); every service must write the ledger inside its transaction or reconcile later (a crash between GitHub merge and ledger write leaves a ghost); heavy migration; a cleanup/retention job. Also no entity currently lets the request be "re-derived" after the fact, so this is a lot of new machinery for ~15 kinds.

### C. Pure function + derive pending from entity state, with a few request columns (recommended)
The function above, run in two places with the same table: server-side `Derive` when rendering a page or SSE patch (so reload/second device agree), and `Next` in the act handler when it answers a click. Where an entity has no durable trace of "request accepted", add one narrow column on that entity, not a ledger:
- `pull_request.merge_requested_at` (+ `merge_request_id`, `merge_error`): set in the act handler *before* calling GitHub, cleared on `markMerged`; failed attempts store a reason code. This makes "merging" a stored fact, and turns the merge into a two-phase flow (accept -> background merge) so the HTTP request never has to hold GitHub.
- `run.cancel_requested_at` (state `cancelling` already exists and is the proof; no column needed).
- `runner`: state `launching`, `draining`, `cordoned` already exist; Launch/Drain/Terminate derive from them.
- `question`/`task review`: the click is atomic (one transaction), so `processing` is only the request flight; the proof is `question.state`.
Only PR merge, Smart Merge (the resolve run is the entity; the proof is `run.kind='resolve'` queued/running) and request-changes (the revise run) need anything new.
- Good: single source of truth; pure testable core; reload and second device free; matches Datastar (server renders state, SSE patches it); no ledger to reconcile; additive migrations.
- Bad: each kind needs a `Derive`/proof written carefully; two-phase merge is a behaviour change (the request returns before GitHub answers), and failures must be stored (`merge_error`) to survive a reload; some actions (settings saves) have no useful `Derive` and degrade to client-only `processing`.

### D. Make everything synchronous but optimistic-free (just better errors)
Keep one request per action, improve toasts into inline error blocks.
- Good: trivial. Bad: still no processing across reload, no timeout, no stale state, and requests that outlive proxies (Launch, merge on a slow GitHub) still hang the UI. Rejected as insufficient, but its error classification is reused in C.

## Decision

Adopt **C**. The state machine is one pure Go package, `internal/actions`, with `Next` and `Derive` and a kind table. Persisted pending state lives on the entity the action is about, added only where the entity has no durable trace (PR merge; nothing else initially). The act handler returns an `ack` quickly for slow kinds (merge, Smart Merge, launch, drain), does the slow part in a tracked goroutine or the existing scheduler, and the result arrives as an entity change over SSE. Fast atomic kinds stay synchronous and only gain `failed`/`superseded` rendering.

Rendering: one template partial `actctl` (`{{template "actctl" (ctl $kind $entity)}}`) producing the button/chip for any `Status`. Datastar stays in charge of the in-flight moment (`data-indicator`, `data-attr:disabled`, `data-show`); everything after `ack` is server-rendered from `Derive`, so there is no custom JS. `.card.busy` becomes `data-status="processing"` styling on the same CSS class; `.spin` is reused. The `dropEl` removal script and the per-kind `b<key>`/`c<key>` signals are deleted as each pattern migrates.

### Why not B
It is the right shape only if we later want a user-visible activity history across all actions. C leaves room for it (a log row per action already exists in `log_entry`); B can be added later without changing the UI contract because the contract is `Status`.

## Per-kind definitions (the table)

`Proof` = entity state that confirms. `TO` = timeout before `timed_out`. R = retry allowed, C = cancel allowed.

| Kind (pattern) | Proof of success | Superseded when | Known failures (reason -> next step) | TO | R / C |
|---|---|---|---|---|---|
| **P1 PR merge** (approve, "Merge") | `pull_request.state='merged'` | PR merged/closed elsewhere | conflicted -> *Smart Merge*; checks/ruleset (405/409/422) -> link to PR + *Check again*; approval required & no reviewer App -> Settings link; auth -> existing App link; 5xx -> *Retry* | 60 s | R yes / C no (once sent to GitHub) |
| **P1 PR close** | `state='closed'` | merged/closed elsewhere | 404 -> superseded; 5xx -> Retry | 30 s | R / no |
| **P1 request changes** | revise run exists for the PR (`run.kind='revise'`, not terminal) | PR no longer open | empty text -> inline error on the field; run not queueable (archived project) -> reason | 20 s | R / no |
| **P1 Smart Merge** | resolve run `queued..running`, then PR `mergeable != conflicted` | run already in progress -> shown as processing (not an error) | nothing to resolve -> "No conflicts; branch updated"; run ended and still conflicted -> failed "Agent could not resolve" + *Open PR* | 15 min (run) | R after failure / C = cancel resolve run |
| **P2 Answer / accept / reopen question, task review** | `question.state='answered'` (+ answer text) | already answered/cancelled | Conflict -> superseded "Answered elsewhere: <answer>" (card shows who/when) | 10 s | no / no |
| **P2 plan apply, proposal-apply** | proposal consumed / subtree changed | proposal expired (`SubtreeRev` moved) | expired -> "Outdated; ask again" button | 30 s | no / no |
| **P3 task state** (complete, reopen, cancel, delete, queue, dequeue, split, move, queue-all, needs) | `task.state` / `run` row as expected | task already in that state | locked/not queueable -> reason from service | 10 s | yes (idempotent) / no |
| **P4 run cancel** | `run.state` in `cancelled`/terminal | already terminal | runner unreachable -> `cancelling` shown as processing | 2 min | no / no. Timed out -> "Runner has not answered; *Check again*" |
| **P4 run retry** | new run row for the task | task not retryable | reason from service | 10 s | yes / no |
| **P5 fleet: launch** | runner row reaches `idle`/`active` | n/a | cloud quota/IAM error -> reason + link to fleet; never registers -> timed out | 5 min | R after failure or timeout / C = terminate |
| **P5 fleet: drain, terminate, cordon, uncordon, adopt** | `runner.state` changed (`draining`, `terminated`, cordon flag), adopt: runner registered | already gone | instance not found -> superseded | drain: until idle (no TO, show "waiting for N sessions"); terminate 3 min; cordon 10 s; adopt 30 min (token lifetime) | terminate yes / drain C = uncordon |
| **P6 settings & project CRUD** (archive/restore, edit, repo link/unlink, notes, limit, template/agent save/delete, account add/remove/check) | row saved / reread equals request; archive: `archived_at` | stale version (template/agent) -> "Changed elsewhere, reload" | archive refused with active runs -> list of runs (existing design 3c6W9Q); link repo: App not installed -> existing link; account check -> result shown inline | 10 s (account check 30 s) | yes / no |
| **Ask agent** (task-ask, project-ask) | thread message from the agent appears | n/a | agent error -> inline message with *Retry* | 5 min | R / C |

## Audit: actions behind the UI, API and CLI

Starting from the `actions` map in `web.go` (37 actions, all through `/ui/act/{name}`); API/CLI call the same services (`wrangl pr`, `question`, `run`, `runner`, `task`, `project`, `repo`, `fleet`, `template`, `agentdef`). Latency is typical; "UI today" is what the owner sees.

| Group / action | Behind it | Latency | Can go wrong | UI today | Pattern |
|---|---|---|---|---|---|
| PR approve / Merge | optional review + GitHub merge + DB tx | 1–5 s, up to 30 s | conflicts, checks, ruleset, stale, auth | busy dim, then toast; card removed on success | P1 |
| PR close | GitHub close + DB tx | 1–3 s | stale | same | P1 |
| PR request changes | GitHub review (best effort) + queue revise run | <1 s | empty text, archived | busy, toast | P1 |
| Smart Merge (`pr-resolve`) | refresh mergeable, queue resolve run or update-branch | seconds, then minutes | already running, nothing to do | toast "queued"; card unchanged | P1 |
| Answer question / task review accept / reopen | `runs.Answer` tx | <1 s | answered elsewhere (Conflict toast) | card removed on conflict | P2 |
| Plan apply / proposal apply | in-process allowlisted action | <1 s | expired proposal | toast | P2 |
| Task queue, queue-all, dequeue, queue-move | create/cancel queued run, set rank | <1 s | locked, not queueable | toast | P3 |
| Task complete, reopen, cancel, delete | `stack` service, recompute | <1 s | blocked by children/runs | toast; delete navigates | P3 |
| Task new, edit, split, plan, needs | stack service; plan launches an agent run | <1 s; plan minutes | validation | toast / form errors | P3 (plan: P4-like, run is the proof) |
| Run cancel | mark `cancelling`, tell runner | s–min | runner offline | toast "cancelling" | P4 |
| Run retry (CLI/API) | new queued run | <1 s | not retryable | n/a in UI | P4 |
| Runner cordon/uncordon/drain/terminate | DB flag; terminate = cloud API | cordon <1 s; terminate 5–60 s | cloud error, gone | toast | P5 |
| Fleet launch | cloud launch + runner registers | 30 s–5 min | quota, IAM, never registers | toast "launching"; row appears | P5 |
| Instance terminate / adopt | cloud API / token mint | 5–60 s / <1 s | not found; adopt token expires 30 min | toast / result block | P5 |
| Project new, save, limit, archive, restore | DB | <1 s | slug taken; archive refused by active runs | toast; archive sheet | P6 |
| Repo link / unlink / notes | GitHub lookup + DB | 1–3 s | App not installed | toast with link | P6 |
| Template save, agent save/delete | DB, runners re-apply later | <1 s | invalid | toast | P6 |
| Account add / remove / check | GitHub App check | 1–5 s | not installed, revoked | toast with link | P6 |
| Ask agent (task/project) | controller-side boxed `claude -p` | 10 s–5 min | agent error, limit | thread shows reply | Ask (own row in table) |

Reduction: 6 patterns + ask cover all 37 actions.

## UX per state (patterns)

Shared rules: the control that was clicked is replaced by a status line in the **same place** (no layout jump); other controls on the card that conflict are disabled with a title. The card gets `data-status`; `.card.busy` styling applies for `processing`. Destructive ones keep the inline confirm sheet.

**P1 PR (Inbox card / project page).** Mockup of one card through the states:

```
idle           [ Merge ] [ Request changes ] [ Close ]
processing     ⟳ Merging… asking GitHub (12s)         [ ] [ ] [ ]  disabled, card dimmed
                  (same text on reload and on a second device, derived from merge_requested_at)
succeeded      ✓ Merged. Task done.  — card leaves after 2 s; row flashes in Done
failed:conflict ⚠ Merge conflicts with main.   [ Smart Merge ]  [ Open PR ]
failed:checks  ⚠ GitHub blocked the merge: required checks are failing.  [ Check again ] [ Open PR ]
failed:5xx     ⚠ GitHub did not answer (502).   [ Retry ]
superseded     ○ Already merged on GitHub (3 min ago). Nothing to do.      [ Dismiss ]
timed_out      … Still working — GitHub has not answered after 60 s.   [ Check again ]
Smart Merge    ⟳ Smart Merge: an agent is resolving conflicts (run #81, 4 min)   [ View run ] [ Cancel ]
```

**P2 Question / review.** processing: "Sending your answer…" on the button (existing `.spin`). success: card leaves. superseded: card collapses to one line "Answered elsewhere: *yes, go ahead* · by you on phone · 2 min ago" with Dismiss; never the raw `Conflict:` error. failed: inline reason under the field, text preserved.

**P3 Task state.** Button shows spinner + verb ("Completing…"); success flashes the row (existing `wrFlash`); failure is a one-line reason under the control with the next step when there is one ("Has 2 open subtasks. *Show them*"); superseded: row re-renders from the entity and a quiet "Already done" note.

**P4 Run cancel / retry.** processing: chip "Cancelling… waiting for the runner" (derived from `run.state='cancelling'`); timed out: "Runner has not answered in 2 min. *Check again* · *Mark failed*" (the second only if the controller already offers force-fail; otherwise omitted); success: state pill updates.

**P5 Fleet.** Row shows "Launching… instance i-… starting (45s)", survives reload because `runner.state='launching'`. Timed out at 5 min: "No runner registered. *Terminate* · *Launch again*". Drain: "Draining — waiting for 2 sessions" with *Undo* (uncordon). Terminate: inline confirm, then "Terminating…".

**P6 Settings / CRUD.** Save button: spinner then "Saved ✓" for 2 s; failure inline beside the field with next step (App not installed -> *Install the App*); archive refusal keeps the existing design (3c6W9Q).

**Timeouts, everywhere.** Never a silent spinner longer than the kind's TO; after it, "Still working" plus *Check again* (and *Cancel* if allowed). A late `observed` flips the line to success without a click.

Mockup note: these are text mockups; a visual prototype (`docs/design/t8AZpr/prototype.html`) of P1's eight states at desktop and phone widths accompanies the first implementation PR, as with 3c6W9Q, and the owner can amend this ADR from it.

## Consequences

Good:
- One definition of what "done", "stuck" and "already handled" mean; cheap to add a new action (a table row and copy).
- Reload, second device, and SSE reconnect all show the same state, because the UI is a function of stored entities.
- The raw-error class of bug ("Conflict: question 16 is answered") disappears; stale is a first-class, friendly state.
- Ad-hoc `data-class:busy`, `b<key>`/`c<key>` signals, `dropEl` and `githubToast` special cases are removed as each pattern migrates.

Costs and risks:
- PR merge becomes two-phase (accept, then background merge). The background worker must be crash-safe: on startup, any PR with `merge_requested_at` set and state `open` is re-evaluated (ask GitHub whether it merged; if so `markMerged`, else store `merge_error`). That reconcile is the riskiest piece and gets its own test.
- One migration (`pull_request.merge_requested_at`, `merge_request_id`, `merge_error`); additive, nullable.
- Every kind needs a correct `Derive`. Wrong proofs show wrong states, so each row has a table test and the e2e cases below.
- CLI/API callers are unchanged (they still get the synchronous result for fast kinds). For slow kinds the API returns 202 with the entity, and `wrangl` polls the entity (`pr merge --wait` default on).
- More SSE traffic is not expected: patches already fire on these entity kinds (`inbox`: Question, PR, Task, Run, Doc).

## Implementation plan (small PRs, each independently shippable)

1. `internal/actions`: types, `Next`, `Derive`, kind table skeleton, and the state-table tests (all events x states x classes). No UI.
2. `actctl` template partial + CSS (`data-status`, reuse `.busy`/`.spin`) + visual prototype; migrate the Inbox question/answer card (P2), deleting its bespoke busy/stale code.
3. PR pattern P1: migration, two-phase merge, reconcile on startup, Smart Merge-as-next-step; tests: processing -> success, failure with next step (conflict -> Smart Merge), stale, reload while processing, two sessions (Playwright, same pattern as the existing e2e), timeout with fake clock.
4. P3 task actions and P4 run actions.
5. P5 fleet and runners.
6. P6 settings/projects/accounts.
7. Remove the old `dropEl`/`githubToast`/busy paths once no action uses them.

Each PR ends with the audit table row marked migrated.

## Open questions for the owner

1. Is the two-phase merge (return before GitHub answers) acceptable, or must merge stay synchronous with only the state machine layered on top (simpler, but a reload during the merge shows idle)?
2. Timeouts above are guesses from the latencies observed; are they right for your GitHub/cloud setup?
3. Should superseded cards auto-remove after a delay, or wait for Dismiss? (Draft: Dismiss, so the explanation can be read.)
4. Should the CLI wait for slow kinds by default?

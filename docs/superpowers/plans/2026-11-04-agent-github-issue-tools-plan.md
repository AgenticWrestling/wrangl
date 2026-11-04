# Agent GitHub issue tools: implementation plan

Spec: [spec](../specs/2026-11-04-agent-github-issue-tools.md). ADR: [0004](../../wrangl/adr/0004-agent-github-issue-tools.md).
All paths are under `cmd/wrangl/internal/`. Every task is tests first: write the failing test, see it
fail, implement, see it pass. Before each commit run `gofmt -l .` (must print nothing) and
`go test ./...` (or `task test`) from the repo root. Never loosen an existing scope check.

## Phase A: read tools (no GitHub permission change)

Status: A1-A7 built (see spec section 11 for the differences); A8 periodic sync is not built.

**A1. Migration and queries.** Files: `db/migrations/00029_issue_sync.sql`, `db/queries/*.sql` (issue
upsert with `WHERE` newer guard, search, get-by-repo-number, comment upsert/list), regenerate `dbgen`.
Test first: `db/issue_test.go`: older `gh_updated_at` does not overwrite newer; empty overwrites;
FTS finds a title/body word after insert, update and delete; porter stemming. Check:
`go test ./db/...`.

**A2. gh layer.** Files: `controller/gh/gh.go` (extend `Issue`: Author, Labels, CreatedAt, UpdatedAt,
Comments; add `IssueAPI`: `GetIssue`, `SearchIssues`, `IssueComments`), `client.go`, `fake.go`/new
`fake_issue.go`, `multi.go`. Test first: `client_test.go` against `httptest` (PRs filtered out, pagination,
labels mapping, 404 -> not found, rate-limit headers), `fake` behaviour test. Check: `go test ./controller/gh/...`.

**A3. Row mapping and sync.** Files: `controller/projects/projects.go` (`SyncIssues` uses the shared
mapper and the guarded upsert; normalise `synced_at`), `controller/prs/prs.go` (webhook uses the same
mapper, guard, drops PR-shaped payloads). Test first: webhook with older/equal/newer `updated_at`;
`SyncIssues` after a newer webhook does not regress; `synced_at` format consistent. Check:
`go test ./controller/projects/... ./controller/prs/...`.

**A4. Untrusted wrapper.** Files: new `controller/issuetools/untrusted.go`. Test first: wraps fields,
escapes embedded closing tags, covers title/body/comment/label/author; golden strings. Check:
`go test ./controller/issuetools/...`.

**A5. Read service and scope resolver.** Files: `controller/issuetools/service.go` (`Search`, `Get`,
`Comments`, repo resolver from `ListRepos(scope.ProjectID)`, FTS query sanitiser, live fallback, staleness,
rate bucket). Test first: other-project repo -> NotFound; unlinked repo; omitted repo with one/many
linked repos; hostile FTS input (`"`, `*`, `NEAR(`, `OR`) never errors; live fallback upserts; stale
refresh; rate limit returns retry_after; PRs never returned. Check: `go test ./controller/issuetools/...`.

**A6. Catalogue and dispatch.** Files: `stacktools/tools.go` (`issue_search`, `issue_get`,
`issue_comments`; allow for work/revise/user/review/plan), `controller/runs/tools.go` (`case` delegating
to `ToolHandlers`), wiring in the controller's constructor (where `submit_review` is registered).
Test first: `stacktools` test that each kind's list contains exactly the expected issue tools and
descriptions mention untrusted data; `runs/tools_test.go` end-to-end through `ExecTool` with run scopes;
existing tool tests unchanged. Check: `go test ./stacktools/... ./controller/runs/...`.

**A7. Runner path and e2e.** Files: verify `runner` shim/daemon pass new tools (they relay by catalogue);
`e2e/` test: fake agent calls `issue_search`, gets wrapped results over real NATS; confirms no GitHub token
in the runner environment. Check: `go test ./e2e/... ./runner/...`.

**A8. Docs and periodic sync.** Files: `docs/wrangl/agents.md` (tool list), a ticker in the controller for
`since=` sync with test using a fake clock. Check: `go test ./controller/...`; `gofmt -l .`.

Phase A exit: `go test ./...` green; agent can search/read; no permission change.

## Phase B: writes, then sync (blocked until the owner raises `issues` to write)

Status (PH6nkJ): B1.1, B2.1-B2.4 and the INSTALL.md upgrade section are built. Not built: B1.2 permission status page, B3. `issue_link_task` is not built; `created_by_run` is not stored (the write is logged as `issue write` with actor, op, ref, url). Label names are passed through, not checked against existing labels.

### B1 Permission plumbing

**B1.1 Manifests.** `controller/ghsetup/ghsetup.go`: author `issues: write` (+ `issue_comment` event).
Test first in `ghsetup_test.go`: manifest permissions/events for author and reviewer. Check: `go test ./controller/ghsetup/...`.

**B1.2 Permission status.** `ghsetup` + `controller/gh`: read `GET /app` and installation `permissions`;
expose status; Setup page text/link in `web/templates/` and API view. Test first with `httptest`:
app has write/installation not accepted; both write; read only. Check: `go test ./controller/ghsetup/... ./web/... ./api/...`.

### B2 Write tools

**B2.1 gh writes.** `gh.go`/`client.go`/`fake_issue.go`: `CreateIssue`, `EditIssue`, `CreateComment`,
`CloseIssue`. Test first: request bodies, 403 -> typed `ErrIssuePermission`, 422, label add/remove. Check: `go test ./controller/gh/...`.

**B2.2 Service writes (write-through).** `controller/issuetools/write.go`. Test first: success updates
the row with GitHub's `updated_at`; GitHub failure leaves DB unchanged; local upsert failure returns
`cached:false`; repo not linked -> NotFound and no GitHub call (assert fake call log empty); size limits;
review/plan kinds refused; permission error mapped to the owner message; rate bucket shared with reads.
Check: `go test ./controller/issuetools/...`.

**B2.3 Catalogue.** `stacktools/tools.go` (`issue_comment`, `issue_create`, `issue_edit`,
`issue_link_task`, `issue_close`), `runs/tools.go` dispatch; `issue_link_task` uses `writable`. Test
first: allow-lists per kind; link in another project refused; link outside assignment forbidden. Check:
`go test ./stacktools/... ./controller/runs/...`.

**B2.4 Approval for close.** `controller/runs/questions.go` (`approvalPayload` gains `Repo`, `Number`;
`resolveApproval` handles `issue_close`), tool path creates question as `task_cancel` does. Test first:
agent call returns `pending_approval`, GitHub untouched; Approve closes and updates row; Deny does
nothing; owner scope acts directly; title quoted in a code block. `issue_delete` stays unimplemented
(spec section 2) with a test that the tool is unknown. Check: `go test ./controller/runs/...`.

### B3 Sync

**B3.1 Echo suppression.** Test first (`controller/prs`): own write followed by the same-`updated_at`
webhook changes nothing and publishes no UI event; a third-party edit later applies; reordered deliveries
ignored; `deleted`/`transferred` handled. Implement the remaining webhook cases. Check:
`go test ./controller/prs/...`.

**B3.2 Comment cache.** `issue_comment` webhook, comment upsert, invalidation by `comment_count`. Test
first with fake. Check: `go test ./controller/issuetools/... ./controller/prs/...`.

**B3.3 Task link UX.** `api/work.go`, `cli/issue.go`, `web`: show issue state beside linked task, note when
the issue closed; no automatic state change (test asserting completing a task never calls the GitHub
fake). Check: `go test ./api/... ./cli/... ./web/...`.

**B3.4 Docs.** INSTALL.md section on upgrading existing Apps (spec section 7), `agents.md`. Check:
`gofmt -l .`, `go test ./...`.

Phase B exit: owner accepted the permission on the live Apps; manual smoke: agent comments on a test
issue, webhook echo ignored, close asks for approval.

# dh — M1: Command Tree, Config, Database, Controller Skeleton

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Spec:** `docs/superpowers/specs/2026-10-31-dh-controller-runner-design.md`

**Goal:** Stand up the `dh` binary with the full cobra command tree (every command present, most
stubbed), config loading, the SQLite database package (goose migrations + sqlc, full v1 schema),
and a `controller serve` that opens the DB, starts an embedded NATS server with JetStream, and
answers `/healthz` on loopback and a unix socket.

**Architecture:** `cmd/dh/main.go` builds the root; `cmd/dh/internal/cli` holds one file per
noun; `cmd/dh/internal/db` owns schema, queries and the generated `dbgen` package;
`cmd/dh/internal/controller` wires the server. M1 adds no NATS auth (M2) and binds NATS to
`127.0.0.1` only.

**Tech Stack:** Go (module toolchain), `spf13/cobra`, `modernc.org/sqlite`, `pressly/goose/v3`,
`sqlc` v1.31+, `nats-io/nats-server/v2`, `nats-io/nats.go`, `gopkg.in/yaml.v3`.

## Global Constraints

- Module path: `github.com/nreid/devhost`; binary `build/dh`; `CGO_ENABLED=0`.
- Generated code (`cmd/dh/internal/db/dbgen`) is committed; `task sqlc` regenerates it and CI/`task test` must fail if it is stale.
- Every stub command returns `cli.ErrNotImplemented` (exit 1, message `"<cmd path>: not implemented yet (milestone Mx)"`).
- `go vet ./...` and `go test ./...` pass at the end of every task.

## Verified up front (2026-10-31)

The schema in Task 4 was run in a scratch module before this plan was written:
`goose up` → inserts → sqlc-generated `ListActiveRuns` and `SearchLogs` → FK rejection → `goose down`,
all on `modernc.org/sqlite` with `CGO_ENABLED=0`. Findings that shape the plan:

- FTS5 works on modernc without cgo.
- sqlc v1.31.1 parses `CREATE VIRTUAL TABLE … USING fts5` but **rejects `WHERE log_line_fts MATCH ?`**
  (`column "log_line_fts" does not exist`). Use the column form `WHERE f.message MATCH ?` —
  equivalent here because the index has one column.
- `question.answer_option_id` references `question_option`, which is created later in the same
  migration; SQLite accepts the forward FK reference.

---

### Task 1: Dependencies, scaffold, Taskfile

**Files:**
- Modify: `go.mod`, `go.sum`, `Taskfile.yml`
- Create: `cmd/dh/main.go`, `cmd/dh/internal/cli/root.go`

- [ ] **Step 1:** `go get github.com/spf13/cobra modernc.org/sqlite github.com/pressly/goose/v3 github.com/nats-io/nats-server/v2 github.com/nats-io/nats.go gopkg.in/yaml.v3`
- [ ] **Step 2:** Add Taskfile targets (keep the existing `ssm` ones unchanged):

```yaml
  build-dh:
    desc: Build dh for host + linux/{arm64,amd64} runner binaries
    cmds:
      - mkdir -p build
      - CGO_ENABLED=0 go build -o build/dh ./cmd/dh
      - CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o build/dh-linux-aarch64 ./cmd/dh
      - CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o build/dh-linux-x86_64 ./cmd/dh
  sqlc:
    desc: Regenerate sqlc code
    dir: cmd/dh/internal/db
    cmds: [sqlc generate]
  sqlc-check:
    dir: cmd/dh/internal/db
    cmds: [sqlc diff]
```

  Runner binaries are named by `uname -m` output (`aarch64`, `x86_64`) so user-data can fetch
  `dh-linux-$(uname -m)` with no mapping.
- [ ] **Step 3:** `main.go` mirrors `cmd/ssm/main.go`: `signal.NotifyContext`, `cli.NewRoot().ExecuteContext(ctx)`, print error, exit 1.

### Task 2: Full command tree with stubs

**Files:**
- Create: `cmd/dh/internal/cli/{root,controller,runner,tui,project,repo,issue,task,run,logs,pr,question,template,fleet,push,config}.go`
- Create: `cmd/dh/internal/cli/tree_test.go`

**Interfaces:**
```go
var ErrNotImplemented = errors.New("not implemented yet")

// NewRoot builds the whole tree. Global persistent flags:
//   --config string, --controller string, -o/--output table|json|yaml (default table)
func NewRoot() *cobra.Command

// stub returns a RunE that fails with "<path>: not implemented yet (milestone <m>)".
func stub(milestone string) func(*cobra.Command, []string) error
```

- [ ] **Step 1: failing test** — `tree_test.go` walks `NewRoot()` and asserts every path below exists, has a non-empty `Short`, and that `--output` rejects values other than `table|json|yaml`:

```go
var wantPaths = []string{
	"controller init", "controller serve", "controller migrate", "controller backup",
	"runner bootstrap", "runner run", "runner hook", "runner mcp", "runner status",
	"tui",
	"project create", "project list", "project show", "project edit", "project delete",
	"repo link", "repo unlink", "repo list",
	"issue sync", "issue list", "issue show", "issue to-task",
	"task create", "task list", "task show", "task cancel", "task retry",
	"run list", "run show", "run cancel", "run attach", "run diff", "run files", "run cat",
	"logs search", // `logs <run>` is the logs command itself
	"pr list", "pr show", "pr approve", "pr request-changes", "pr close",
	"question list", "question show", "question answer",
	"template create", "template list", "template show", "template edit", "template delete",
	"fleet list", "fleet show", "fleet launch", "fleet cordon", "fleet uncordon",
	"fleet drain", "fleet terminate", "fleet enroll-token",
	"push test",
	"config use", "config show",
}
```

- [ ] **Step 2:** Implement each noun file. Declare every flag from spec §3 now, even on stubs, so `--help` is the reviewable design. Use `cobra.ExactArgs`/`RangeArgs` matching the spec. Mark milestone per stub (e.g. `task create` → M4, `fleet launch` → M3, `tui` → M7).
- [ ] **Step 3:** Set `Hidden: true` on `runner hook` and `runner mcp` (they are machine entrypoints).
- [ ] **Step 4:** `go test ./cmd/dh/...` green; eyeball `build/dh --help`, `build/dh task create --help`.

### Task 3: Config loading

**Files:**
- Create: `cmd/dh/internal/config/{controller,client}.go` and `_test.go`

**Interfaces:**
```go
type Controller struct {
	DataDir string `yaml:"data_dir"`
	Listen  struct{ HTTP, Unix, NATS, Enroll string } `yaml:"listen"`
	AWS     struct {
		Region                string   `yaml:"region"`
		SubnetIDs             []string `yaml:"subnet_ids"`
		SecurityGroupID       string   `yaml:"security_group_id"`
		RunnerInstanceProfile string   `yaml:"runner_instance_profile"`
	} `yaml:"aws"`
	GitHub     struct{ AuthorApp, ReviewerApp GitHubApp } `yaml:"github"`
	Cloudflare struct{ Team, AUD, OwnerEmail string }    `yaml:"cloudflare"`
	Web        struct{ PublicURL string }                `yaml:"web"`
}
type GitHubApp struct{ ID int64; PrivateKey string `yaml:"private_key"` }

func LoadController(path string) (*Controller, error) // applies defaults, validates
func DefaultController() *Controller                    // data_dir /var/lib/dh, http 127.0.0.1:8080,
                                                        // unix /run/dh/api.sock, nats 127.0.0.1:4222, enroll 127.0.0.1:4223
type Client struct {
	Controller     string `yaml:"controller"`
	CFServiceToken *struct{ ID, Secret string } `yaml:"cf_service_token,omitempty"`
}
func LoadClient(path string) (*Client, error) // default path: $XDG_CONFIG_HOME/dh/config.yaml
```

- [ ] **Step 1: failing tests** — defaults applied for missing keys; unknown keys rejected (`yaml.Decoder.KnownFields(true)`); missing file → defaults for client, error for controller unless `--data-dir` given.
- [ ] **Step 2:** Implement. Flag precedence: flag > env (`DH_CONFIG`, `DH_CONTROLLER`) > file > default.
- [ ] **Step 3:** Wire `config show` (prints effective client config, secret redacted) and `config use <url>` (writes it) — the first two real commands.

### Task 4: Database package

**Files:**
- Create: `cmd/dh/internal/db/migrations/00001_init.sql` (below, verbatim)
- Create: `cmd/dh/internal/db/queries/{project,runner,template,task,run,log,question,push}.sql`
- Create: `cmd/dh/internal/db/sqlc.yaml`, `cmd/dh/internal/db/db.go`, `cmd/dh/internal/db/db_test.go`
- Generate: `cmd/dh/internal/db/dbgen/`

**Interfaces:**
```go
type DB struct {
	W *sql.DB // single-connection writer (SetMaxOpenConns(1))
	R *sql.DB // reader pool
	Q  *dbgen.Queries // bound to W
	RQ *dbgen.Queries // bound to R
}
// Open creates the file if needed, applies pragmas (foreign_keys, WAL, busy_timeout=5000,
// synchronous=NORMAL) via DSN _pragma params on both pools, and runs goose Up.
func Open(ctx context.Context, path string) (*DB, error)
func (d *DB) Tx(ctx context.Context, fn func(*dbgen.Queries) error) error // on W
func (d *DB) Close() error
// Migrate exposes goose up/down/status for `dh controller migrate`.
func Migrate(ctx context.Context, path, direction string) error
```

- [ ] **Step 1:** Write the migration:

```sql
-- +goose Up
CREATE TABLE runner_template (
    id              INTEGER PRIMARY KEY,
    name            TEXT    NOT NULL UNIQUE,
    description     TEXT    NOT NULL DEFAULT '',
    mise_toml       TEXT    NOT NULL,
    setup_script    TEXT    NOT NULL DEFAULT '',
    agent_json      TEXT    NOT NULL,
    min_vcpu        INTEGER NOT NULL CHECK (min_vcpu > 0),
    min_ram_mib     INTEGER NOT NULL CHECK (min_ram_mib > 0),
    min_disk_gib    INTEGER NOT NULL CHECK (min_disk_gib > 0),
    arch            TEXT    NOT NULL DEFAULT 'arm64' CHECK (arch IN ('arm64', 'x86_64')),
    instance_types  TEXT    NOT NULL DEFAULT '[]',
    ami             TEXT    NOT NULL DEFAULT '/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64',
    max_runners     INTEGER NOT NULL DEFAULT 1 CHECK (max_runners >= 0),
    idle_timeout_s  INTEGER NOT NULL DEFAULT 1800,
    version         INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE runner (
    id                INTEGER PRIMARY KEY,
    name              TEXT    NOT NULL UNIQUE,
    template_id       INTEGER NOT NULL REFERENCES runner_template (id),
    template_version  INTEGER NOT NULL DEFAULT 0,
    instance_id       TEXT    UNIQUE,
    instance_type     TEXT    NOT NULL DEFAULT '',
    az                TEXT    NOT NULL DEFAULT '',
    private_ip        TEXT    NOT NULL DEFAULT '',
    arch              TEXT    NOT NULL DEFAULT '',
    vcpu              INTEGER NOT NULL DEFAULT 0,
    ram_mib           INTEGER NOT NULL DEFAULT 0,
    disk_gib          INTEGER NOT NULL DEFAULT 0,
    nkey_public       TEXT    UNIQUE,
    state             TEXT    NOT NULL DEFAULT 'launching' CHECK (state IN (
                          'launching', 'enrolling', 'provisioning', 'idle', 'busy',
                          'cordoned', 'draining', 'offline', 'terminated')),
    max_concurrent    INTEGER NOT NULL DEFAULT 1,
    last_heartbeat_at TEXT,
    last_metrics_json TEXT    NOT NULL DEFAULT '{}',
    created_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    terminated_at     TEXT
);
CREATE INDEX runner_state ON runner (state);

CREATE TABLE enrollment_token (
    id           INTEGER PRIMARY KEY,
    token_hash   BLOB    NOT NULL UNIQUE,
    template_id  INTEGER NOT NULL REFERENCES runner_template (id),
    instance_id  TEXT,
    runner_id    INTEGER REFERENCES runner (id),
    expires_at   TEXT    NOT NULL,
    used_at      TEXT,
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE project (
    id                   INTEGER PRIMARY KEY,
    slug                 TEXT    NOT NULL UNIQUE,
    name                 TEXT    NOT NULL,
    description          TEXT    NOT NULL DEFAULT '',
    default_template_id  INTEGER REFERENCES runner_template (id),
    auto_merge           INTEGER NOT NULL DEFAULT 0 CHECK (auto_merge IN (0, 1)),
    protected_paths      TEXT    NOT NULL DEFAULT '[".github/**","**/migrations/**","go.mod","**/*.tf","Dockerfile*"]',
    max_auto_merge_lines INTEGER NOT NULL DEFAULT 200,
    created_at           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at           TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE repo (
    id                  INTEGER PRIMARY KEY,
    project_id          INTEGER NOT NULL REFERENCES project (id) ON DELETE CASCADE,
    owner               TEXT    NOT NULL,
    name                TEXT    NOT NULL,
    default_branch      TEXT    NOT NULL DEFAULT 'main',
    gh_installation_id  INTEGER,
    created_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (project_id, owner, name)
);

CREATE TABLE issue (
    id         INTEGER PRIMARY KEY,
    repo_id    INTEGER NOT NULL REFERENCES repo (id) ON DELETE CASCADE,
    gh_number  INTEGER NOT NULL,
    title      TEXT    NOT NULL,
    body       TEXT    NOT NULL DEFAULT '',
    state      TEXT    NOT NULL CHECK (state IN ('open', 'closed')),
    url        TEXT    NOT NULL,
    synced_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (repo_id, gh_number)
);

CREATE TABLE task (
    id           INTEGER PRIMARY KEY,
    project_id   INTEGER NOT NULL REFERENCES project (id) ON DELETE CASCADE,
    repo_id      INTEGER NOT NULL REFERENCES repo (id),
    issue_id     INTEGER REFERENCES issue (id) ON DELETE SET NULL,
    template_id  INTEGER NOT NULL REFERENCES runner_template (id),
    title        TEXT    NOT NULL,
    prompt       TEXT    NOT NULL,
    priority     INTEGER NOT NULL DEFAULT 0,
    state        TEXT    NOT NULL DEFAULT 'open' CHECK (state IN (
                     'open', 'in_progress', 'in_review', 'done', 'cancelled')),
    created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX task_project_state ON task (project_id, state);

CREATE TABLE run (
    id           INTEGER PRIMARY KEY,
    task_id      INTEGER NOT NULL REFERENCES task (id) ON DELETE CASCADE,
    runner_id    INTEGER REFERENCES runner (id),
    kind         TEXT    NOT NULL DEFAULT 'work' CHECK (kind IN ('work', 'review', 'revise')),
    attempt      INTEGER NOT NULL DEFAULT 1,
    state        TEXT    NOT NULL DEFAULT 'queued' CHECK (state IN (
                     'queued', 'assigned', 'preparing', 'running', 'needs_input', 'pushing',
                     'succeeded', 'failed', 'cancelling', 'cancelled')),
    branch       TEXT    NOT NULL DEFAULT '',
    base_sha     TEXT    NOT NULL DEFAULT '',
    head_sha     TEXT    NOT NULL DEFAULT '',
    status_text  TEXT    NOT NULL DEFAULT '',
    exit_code    INTEGER,
    error        TEXT    NOT NULL DEFAULT '',
    queued_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    started_at   TEXT,
    finished_at  TEXT
);
CREATE INDEX run_state ON run (state);
CREATE INDEX run_task ON run (task_id);
CREATE INDEX run_runner ON run (runner_id);

CREATE TABLE pull_request (
    id                 INTEGER PRIMARY KEY,
    task_id            INTEGER NOT NULL REFERENCES task (id) ON DELETE CASCADE,
    repo_id            INTEGER NOT NULL REFERENCES repo (id),
    run_id             INTEGER NOT NULL REFERENCES run (id),
    gh_number          INTEGER NOT NULL,
    url                TEXT    NOT NULL,
    head_branch        TEXT    NOT NULL,
    state              TEXT    NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'merged', 'closed')),
    risk               TEXT    CHECK (risk IN ('low', 'medium', 'high')),
    review_summary_md  TEXT    NOT NULL DEFAULT '',
    gates_json         TEXT    NOT NULL DEFAULT '{}',
    decision           TEXT    CHECK (decision IN (
                           'auto_merged', 'user_approved', 'changes_requested', 'closed')),
    created_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (repo_id, gh_number)
);

CREATE TABLE log_line (
    id       INTEGER PRIMARY KEY,
    run_id   INTEGER NOT NULL REFERENCES run (id) ON DELETE CASCADE,
    seq      INTEGER NOT NULL,
    ts       TEXT    NOT NULL,
    stream   TEXT    NOT NULL CHECK (stream IN ('agent', 'runner', 'hook', 'system')),
    level    TEXT    NOT NULL DEFAULT 'info',
    message  TEXT    NOT NULL,
    UNIQUE (run_id, seq)
);

CREATE VIRTUAL TABLE log_line_fts USING fts5 (
    message, content = 'log_line', content_rowid = 'id'
);
-- +goose StatementBegin
CREATE TRIGGER log_line_ai AFTER INSERT ON log_line BEGIN
    INSERT INTO log_line_fts (rowid, message) VALUES (new.id, new.message);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER log_line_ad AFTER DELETE ON log_line BEGIN
    INSERT INTO log_line_fts (log_line_fts, rowid, message) VALUES ('delete', old.id, old.message);
END;
-- +goose StatementEnd

CREATE TABLE hook_event (
    id            INTEGER PRIMARY KEY,
    run_id        INTEGER NOT NULL REFERENCES run (id) ON DELETE CASCADE,
    seq           INTEGER NOT NULL,
    ts            TEXT    NOT NULL,
    name          TEXT    NOT NULL,
    payload_json  TEXT    NOT NULL DEFAULT '{}',
    UNIQUE (run_id, seq)
);

CREATE TABLE question (
    id                INTEGER PRIMARY KEY,
    task_id           INTEGER NOT NULL REFERENCES task (id) ON DELETE CASCADE,
    run_id            INTEGER REFERENCES run (id) ON DELETE SET NULL,
    kind              TEXT    NOT NULL CHECK (kind IN ('agent', 'pr_review', 'system')),
    prompt            TEXT    NOT NULL,
    detail_md         TEXT    NOT NULL DEFAULT '',
    allow_free_text   INTEGER NOT NULL DEFAULT 0 CHECK (allow_free_text IN (0, 1)),
    state             TEXT    NOT NULL DEFAULT 'open' CHECK (state IN (
                          'open', 'answered', 'cancelled')),
    answer_option_id  INTEGER REFERENCES question_option (id),
    answer_text       TEXT    NOT NULL DEFAULT '',
    created_at        TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    answered_at       TEXT
);
CREATE INDEX question_state ON question (state);

CREATE TABLE question_option (
    id           INTEGER PRIMARY KEY,
    question_id  INTEGER NOT NULL REFERENCES question (id) ON DELETE CASCADE,
    position     INTEGER NOT NULL,
    label        TEXT    NOT NULL,
    value        TEXT    NOT NULL,
    description  TEXT    NOT NULL DEFAULT '',
    UNIQUE (question_id, position),
    UNIQUE (question_id, value)
);

CREATE TABLE push_subscription (
    id               INTEGER PRIMARY KEY,
    endpoint         TEXT    NOT NULL UNIQUE,
    p256dh           TEXT    NOT NULL,
    auth             TEXT    NOT NULL,
    user_agent       TEXT    NOT NULL DEFAULT '',
    failures         INTEGER NOT NULL DEFAULT 0,
    last_success_at  TEXT,
    created_at       TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- +goose Down
DROP TABLE push_subscription;
DROP TABLE question_option;
DROP TABLE question;
DROP TABLE hook_event;
DROP TRIGGER log_line_ad;
DROP TRIGGER log_line_ai;
DROP TABLE log_line_fts;
DROP TABLE log_line;
DROP TABLE pull_request;
DROP TABLE run;
DROP TABLE task;
DROP TABLE issue;
DROP TABLE repo;
DROP TABLE project;
DROP TABLE enrollment_token;
DROP TABLE runner;
DROP TABLE runner_template;
```

- [ ] **Step 2:** `sqlc.yaml`:

```yaml
version: "2"
sql:
  - engine: sqlite
    schema: migrations
    queries: queries
    gen:
      go:
        package: dbgen
        out: dbgen
        emit_json_tags: true
        emit_empty_slices: true
```

- [ ] **Step 3:** Queries for M1–M2 only (later milestones add their own): `CreateTemplate`, `GetTemplateByName`, `ListTemplates`, `CreateProject`, `GetProjectBySlug`, `ListProjects`, `CreateRunner`, `ListRunners`, `UpdateRunnerState`, `ListActiveRuns` (join task/project/runner, non-terminal, `priority DESC, queued_at`), `InsertLogLine`, `SearchLogs` (**use `WHERE f.message MATCH ?`**, see "Verified up front"), `CreateQuestion`, `CreateQuestionOption`, `ListOpenQuestions`, `UpsertPushSubscription`.
- [ ] **Step 4: failing test** `db_test.go` — port the verified scratch test: Open on `t.TempDir()`, insert template → project → repo → task → run → two log lines; assert `ListActiveRuns` returns 1, `SearchLogs("disk")` returns exactly the matching line, an FK violation errors, a bad `state` value violates its CHECK, and `Migrate(..., "down")` succeeds.
- [ ] **Step 5:** `task sqlc`, implement `db.go`, test green. Confirm `CGO_ENABLED=0 go test ./cmd/dh/internal/db/` passes.

### Task 5: `controller init` and `controller migrate`

**Files:**
- Create: `cmd/dh/internal/controller/initcmd.go`; modify `cli/controller.go`

- [ ] **Step 1: failing test** — `Init(dataDir, configPath)` on temp dirs creates `data_dir` (0750), `data_dir/bin/`, `data_dir/nats/`, writes a config skeleton only if absent (never overwrites), and creates `dh.db` with migrations applied. Running it twice is a no-op.
- [ ] **Step 2:** Implement; `controller migrate [up|down|status]` calls `db.Migrate`. (NATS keys, VAPID keys, TLS cert generation are added to `init` in M2/M8 — leave a `// M2:` marker.)

### Task 6: `controller serve` skeleton

**Files:**
- Create: `cmd/dh/internal/controller/server.go`, `server_test.go`

**Interfaces:**
```go
type Server struct { /* cfg, db, ns *server.Server, nc *nats.Conn, js jetstream.JetStream, http */ }
func New(cfg *config.Controller) (*Server, error)
// Run opens the DB, starts embedded NATS (JetStream on, StoreDir = data_dir/nats/js,
// listening on cfg.Listen.NATS — 127.0.0.1 in M1), connects in-process
// (nats.InProcessServer), creates streams DH_EVT and DH_PTY if missing (subjects, limits per
// spec §7), serves HTTP on cfg.Listen.HTTP and cfg.Listen.Unix (socket mode 0660), and blocks
// until ctx is cancelled, then shuts down HTTP → NATS → DB in that order.
func (s *Server) Run(ctx context.Context) error
```

- [ ] **Step 1: failing test** — start `Run` in a goroutine with temp data dir, port 0 for HTTP and NATS (`-1` random port for nats-server), unix socket in temp dir. Assert: `GET /healthz` over TCP and over the unix socket returns `200 {"db":"ok","nats":"ok","jetstream":"ok"}`; `js.Stream(ctx, "DH_EVT")` and `"DH_PTY"` exist; cancelling ctx returns nil within 5 s and the socket file is removed.
- [ ] **Step 2:** Implement. `healthz` pings DB (`SELECT 1` on R) and checks `ns.ReadyForConnections` + `js.AccountInfo`.
- [ ] **Step 3:** Wire `dh controller serve`; manual check: `task build-dh && build/dh controller init --data-dir /tmp/dhd --config /tmp/dhd/c.yaml && build/dh controller serve --config /tmp/dhd/c.yaml`, then `curl --unix-socket /tmp/dhd/api.sock http://x/healthz`.

### Task 7: Wrap-up

- [ ] `go vet ./... && go test ./... && task sqlc-check` all green.
- [ ] README: add a short `dh` section (what it is, `task build-dh`, link to the spec). Mark M1 done in the spec's §14 table.

## Out of scope for M1

NATS auth/JWTs, enrolment, any AWS or GitHub calls, HTTP API beyond `/healthz`, TUI, web.

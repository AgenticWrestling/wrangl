# wrangl — M1: Command Tree, Config, Database, Work-Stack Primitives, Controller Skeleton

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Spec:** `docs/superpowers/specs/2026-10-31-wrangl-design.md` (rev 2)

**Goal:** Stand up the `wrangl` binary with the full cobra command tree (every command present,
most stubbed), config loading, the SQLite database package (goose + sqlc, full v1 schema
including the hierarchical work stack), the `stack` primitives (sqids, fractional ranks), and
a `controller serve` that opens the DB, starts embedded NATS with JetStream, and answers
`/healthz` on loopback and a unix socket.

**Architecture:** `cmd/wrangl/main.go` builds the root; `cmd/wrangl/internal/cli` holds one
file per noun; `internal/db` owns schema, queries, generated `dbgen`; `internal/stack` holds
pure work-stack helpers (no I/O in M1); `internal/controller` wires the server. No NATS auth
yet (M2); NATS binds `127.0.0.1` only.

**Tech Stack:** Go (module toolchain), `spf13/cobra`, `modernc.org/sqlite`,
`pressly/goose/v3`, `sqlc` v1.31+, `sqids/sqids-go`, `nats-io/nats-server/v2`,
`nats-io/nats.go` (+ `jetstream`), `gopkg.in/yaml.v3`.

## Global Constraints

- Module path `github.com/nreid/devhost`; binary `build/wrangl`; `CGO_ENABLED=0`.
- `cmd/wrangl/internal/db/dbgen` is generated **and committed**; `task sqlc-check` fails if stale.
- Stub commands return `cli.ErrNotImplemented` → `"<cmd path>: not implemented yet (milestone Mx)"`, exit 1.
- `go vet ./...` and `go test ./...` green at the end of every task.

## Verified up front (2026-10-31)

The migration and queries below were run in a scratch module before this plan was written
(`goose up` → fixtures → every query below → constraint violations → `goose down`, on
`modernc.org/sqlite`, `CGO_ENABLED=0`, sqlc 1.31.1). The two tests in Task 4 are ports of that
scratch module and pass against exactly this SQL. sqlc quirks found and already worked around
in the SQL below — **do not "simplify" them back**:

1. `WHERE log_line_fts MATCH ?` → sqlc: `column "log_line_fts" does not exist`. Use
   `WHERE f.message MATCH ?` (single-column index, equivalent).
2. A recursive CTE referenced only inside `EXISTS(...)` in a FROM-less `SELECT` → sqlc:
   `table alias "up" does not exist`. Put the CTE in `FROM` and use `COUNT(*) > 0`.
3. A parameter compared to a CTE column → same error. Cast it:
   `CAST(sqlc.arg(x) AS INTEGER)`.

---

### Task 1: Dependencies, scaffold, Taskfile

**Files:** modify `go.mod`, `go.sum`, `Taskfile.yml`; create `cmd/wrangl/main.go`, `cmd/wrangl/internal/cli/root.go`

- [ ] **Step 1:** `go get github.com/spf13/cobra modernc.org/sqlite github.com/pressly/goose/v3 github.com/sqids/sqids-go github.com/nats-io/nats-server/v2 github.com/nats-io/nats.go gopkg.in/yaml.v3`
- [ ] **Step 2:** Taskfile targets (existing `ssm` targets unchanged):

```yaml
  build-wrangl:
    desc: Build wrangl for host + linux runner binaries (named by `uname -m`)
    cmds:
      - mkdir -p build
      - CGO_ENABLED=0 go build -o build/wrangl ./cmd/wrangl
      - CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o build/wrangl-linux-aarch64 ./cmd/wrangl
      - CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o build/wrangl-linux-x86_64 ./cmd/wrangl
  sqlc:
    dir: cmd/wrangl/internal/db
    cmds: [sqlc generate]
  sqlc-check:
    dir: cmd/wrangl/internal/db
    cmds: [sqlc diff]
```

- [ ] **Step 3:** `main.go` mirrors `cmd/ssm/main.go`: `signal.NotifyContext`, `cli.NewRoot().ExecuteContext(ctx)`, print error, exit 1.

### Task 2: Full command tree with stubs

**Files:** `cmd/wrangl/internal/cli/{root,controller,runner,tui,project,repo,issue,task,run,logs,pr,question,template,fleet,push,config}.go`, `tree_test.go`

**Interfaces:**
```go
var ErrNotImplemented = errors.New("not implemented yet")
func NewRoot() *cobra.Command  // persistent: --config, --controller, -o/--output table|json|yaml
func stub(milestone string) func(*cobra.Command, []string) error
```

- [ ] **Step 1: failing test** — walk `NewRoot()`; every path exists with non-empty `Short`; `--output bogus` errors; `runner hook` and `runner mcp` are `Hidden`:

```go
var wantPaths = []string{
	"controller init", "controller serve", "controller migrate", "controller backup",
	"controller secret set", "controller secret list", "controller secret rm",
	"runner bootstrap", "runner run", "runner hook", "runner mcp", "runner status",
	"tui",
	"project create", "project list", "project show", "project edit", "project delete",
	"repo link", "repo unlink", "repo list",
	"issue sync", "issue list", "issue show", "issue to-task",
	"task create", "task tree", "task list", "task show", "task edit", "task move",
	"task needs add", "task needs rm", "task queue",
	"task complete", "task reopen", "task cancel", "task delete",
	"run list", "run show", "run cancel", "run attach", "run diff", "run files", "run cat",
	"logs search",
	"pr list", "pr show", "pr approve", "pr request-changes", "pr close",
	"question list", "question show", "question answer",
	"template create", "template list", "template show", "template edit", "template delete",
	"fleet list", "fleet show", "fleet launch", "fleet cordon", "fleet uncordon",
	"fleet drain", "fleet terminate", "fleet enroll-token",
	"push test",
	"config use", "config show",
}
```

- [ ] **Step 2:** Implement every noun with **all flags from spec §3 declared now**, so `--help` is the reviewable design. `task create`/`task move` position flags (`--first|--last|--after|--before`, plus `--root` on move) are mutually exclusive (`MarkFlagsMutuallyExclusive`). Task args are sqids. Stub milestones: stack/task/project/repo/issue → M4, run/logs → M4, question → M5, pr → M6, tui → M7, push → M8, fleet launch/cordon/drain/terminate → M3, fleet list/show/enroll-token → M2, template → M3, runner * → M2.
- [ ] **Step 3:** `go test ./cmd/wrangl/...`; eyeball `build/wrangl task create --help`, `build/wrangl task move --help`.

### Task 3: Config loading

**Files:** `cmd/wrangl/internal/config/{controller,client}.go` + tests

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
	GitHub struct {
		AuthorApp   GitHubApp `yaml:"author_app"`
		ReviewerApp GitHubApp `yaml:"reviewer_app"`
	} `yaml:"github"`
	Cloudflare struct{ Team, AUD, OwnerEmail string } `yaml:"cloudflare"`
	Web        struct{ PublicURL string }             `yaml:"web"`
}
type GitHubApp struct {
	ID            int64  `yaml:"id"`
	PrivateKey    string `yaml:"private_key"`              // relative to data_dir
	WebhookSecret string `yaml:"webhook_secret,omitempty"` // reviewer app only
}
func LoadController(path string) (*Controller, error)
func DefaultController() *Controller // data_dir /var/lib/wrangl; http 127.0.0.1:8080;
                                     // unix /run/wrangl/api.sock; nats 127.0.0.1:4222; enroll 127.0.0.1:4223
func (c *Controller) Path(rel string) string // resolve relative to DataDir

type Client struct {
	Controller     string `yaml:"controller"`
	CFServiceToken *struct{ ID, Secret string } `yaml:"cf_service_token,omitempty"`
}
func LoadClient(path string) (*Client, error) // default $XDG_CONFIG_HOME/wrangl/config.yaml
```

- [ ] **Step 1: failing tests** — defaults fill missing keys; unknown keys rejected (`KnownFields(true)`); relative secret paths resolve under `data_dir`; precedence flag > env (`WRANGL_CONFIG`, `WRANGL_CONTROLLER`) > file > default.
- [ ] **Step 2:** Implement; wire `config show` (secret redacted) and `config use <url>`.

### Task 4: Database package

**Files:** `cmd/wrangl/internal/db/migrations/00001_init.sql`, `queries/*.sql`, `sqlc.yaml`, `db.go`, `db_test.go`, `tree_test.go`; generated `dbgen/`

**Interfaces:**
```go
type DB struct {
	W, R   *sql.DB         // W: SetMaxOpenConns(1); R: reader pool
	Q, RQ  *dbgen.Queries  // bound to W / R
}
// Open: DSN _pragma params (foreign_keys(1), journal_mode(WAL), busy_timeout(5000),
// synchronous(NORMAL)) on both pools; goose Up from embedded migrations.
func Open(ctx context.Context, path string) (*DB, error)
func (d *DB) Tx(ctx context.Context, fn func(*dbgen.Queries) error) error
func (d *DB) Close() error
func Migrate(ctx context.Context, path, direction string) error // up|down|status
```

- [ ] **Step 1:** Migration, verbatim:

```sql
-- +goose Up
CREATE TABLE meta (
    key    TEXT PRIMARY KEY,
    value  TEXT NOT NULL
);

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
    priority             INTEGER NOT NULL DEFAULT 0,
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
    id                  INTEGER PRIMARY KEY,
    sqid                TEXT    UNIQUE,  -- set in the creating tx; immutable (trigger below)
    project_id          INTEGER NOT NULL REFERENCES project (id) ON DELETE CASCADE,
    parent_id           INTEGER REFERENCES task (id) ON DELETE CASCADE,
    rank                TEXT    NOT NULL,  -- fractional index, unique among siblings
    repo_id             INTEGER REFERENCES repo (id),             -- NULL = inherit from ancestor
    issue_id            INTEGER REFERENCES issue (id) ON DELETE SET NULL,
    template_id         INTEGER REFERENCES runner_template (id),  -- NULL = inherit / project default
    title               TEXT    NOT NULL,
    prompt              TEXT    NOT NULL DEFAULT '',
    state               TEXT    NOT NULL DEFAULT 'open' CHECK (state IN (
                            'open', 'in_progress', 'in_review', 'done', 'cancelled')),
    created_by          TEXT    NOT NULL DEFAULT 'user',  -- user | agent:<run id> | system
    completion_summary  TEXT    NOT NULL DEFAULT '',
    completed_run_id    INTEGER,  -- run whose agent completed it (no FK: run is created later)
    created_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    completed_at        TEXT,
    CHECK (parent_id IS NULL OR parent_id <> id)
);
CREATE INDEX task_project_state ON task (project_id, state);
CREATE INDEX task_parent ON task (parent_id);
CREATE UNIQUE INDEX task_rank_child ON task (parent_id, rank) WHERE parent_id IS NOT NULL;
CREATE UNIQUE INDEX task_rank_root  ON task (project_id, rank) WHERE parent_id IS NULL;

-- +goose StatementBegin
CREATE TRIGGER task_sqid_immutable BEFORE UPDATE OF sqid ON task
WHEN old.sqid IS NOT NULL AND new.sqid IS NOT old.sqid BEGIN
    SELECT RAISE(ABORT, 'task.sqid is immutable');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER task_parent_project_ins BEFORE INSERT ON task
WHEN new.parent_id IS NOT NULL
 AND (SELECT project_id FROM task WHERE id = new.parent_id) IS NOT new.project_id BEGIN
    SELECT RAISE(ABORT, 'parent task must be in the same project');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER task_parent_project_upd BEFORE UPDATE OF parent_id, project_id ON task
WHEN new.parent_id IS NOT NULL
 AND (SELECT project_id FROM task WHERE id = new.parent_id) IS NOT new.project_id BEGIN
    SELECT RAISE(ABORT, 'parent task must be in the same project');
END;
-- +goose StatementEnd

CREATE TABLE task_dependency (
    task_id        INTEGER NOT NULL REFERENCES task (id) ON DELETE CASCADE,
    depends_on_id  INTEGER NOT NULL REFERENCES task (id) ON DELETE CASCADE,
    created_by     TEXT    NOT NULL DEFAULT 'user',
    created_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (task_id, depends_on_id),
    CHECK (task_id <> depends_on_id)
);
CREATE INDEX task_dependency_rev ON task_dependency (depends_on_id);
-- +goose StatementBegin
CREATE TRIGGER task_dependency_project BEFORE INSERT ON task_dependency
WHEN (SELECT project_id FROM task WHERE id = new.task_id)
  IS NOT (SELECT project_id FROM task WHERE id = new.depends_on_id) BEGIN
    SELECT RAISE(ABORT, 'precursor must be in the same project');
END;
-- +goose StatementEnd

-- Audit log of work-stack changes. No FK on task: entries outlive deleted tasks.
CREATE TABLE task_event (
    id         INTEGER PRIMARY KEY,
    task_sqid  TEXT    NOT NULL,
    run_id     INTEGER REFERENCES run (id) ON DELETE SET NULL,
    actor      TEXT    NOT NULL,  -- user | agent:<run id> | system
    op         TEXT    NOT NULL CHECK (op IN (
                   'create', 'update', 'move', 'rerank', 'complete', 'reopen', 'cancel',
                   'delete', 'add_precursor', 'remove_precursor')),
    diff_json  TEXT    NOT NULL DEFAULT '{}',
    ts         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX task_event_sqid ON task_event (task_sqid);

CREATE TABLE run (
    id           INTEGER PRIMARY KEY,
    task_id      INTEGER NOT NULL REFERENCES task (id) ON DELETE CASCADE,  -- assignment root
    current_task_id INTEGER REFERENCES task (id) ON DELETE SET NULL,       -- subtask in progress
    runner_id    INTEGER REFERENCES runner (id),
    kind         TEXT    NOT NULL DEFAULT 'work' CHECK (kind IN ('work', 'review', 'revise')),
    attempt      INTEGER NOT NULL DEFAULT 1,
    state        TEXT    NOT NULL DEFAULT 'queued' CHECK (state IN (
                     'queued', 'assigned', 'preparing', 'running', 'needs_input', 'blocked',
                     'pushing', 'succeeded', 'failed', 'cancelling', 'cancelled')),
    branch       TEXT    NOT NULL DEFAULT '',
    session_name TEXT    NOT NULL DEFAULT '',  -- <runner>:<task sqid>
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
    kind              TEXT    NOT NULL CHECK (kind IN ('agent', 'pr_review', 'approval', 'system')),
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
DROP TABLE task_event;
DROP TABLE task_dependency;
DROP TABLE task;
DROP TABLE issue;
DROP TABLE repo;
DROP TABLE project;
DROP TABLE enrollment_token;
DROP TABLE runner;
DROP TABLE runner_template;
DROP TABLE meta;
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

- [ ] **Step 3:** Queries. Split by noun into `queries/{project,run,log,question,task}.sql`. These are verified verbatim — start from them:

```sql
-- name: CreateProject :one
INSERT INTO project (slug, name, description, default_template_id)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: ListProjects :many
SELECT * FROM project ORDER BY slug;

-- name: ListActiveRuns :many
SELECT r.id, r.state, r.kind, r.status_text, r.queued_at, r.started_at,
       t.title AS task_title, p.slug AS project_slug, rn.name AS runner_name
FROM run r
JOIN task t ON t.id = r.task_id
JOIN project p ON p.id = t.project_id
LEFT JOIN runner rn ON rn.id = r.runner_id
WHERE r.state NOT IN ('succeeded', 'failed', 'cancelled')
ORDER BY p.priority DESC, r.queued_at;

-- name: SearchLogs :many
SELECT l.id, l.run_id, l.ts, l.stream, l.message
FROM log_line_fts f
JOIN log_line l ON l.id = f.rowid
WHERE f.message MATCH ?
ORDER BY l.id DESC
LIMIT ?;

-- name: CreateQuestionOption :one
INSERT INTO question_option (question_id, position, label, value, description)
VALUES (?, ?, ?, ?, ?) RETURNING *;

-- name: CreateTask :one
INSERT INTO task (project_id, parent_id, rank, repo_id, template_id, title, prompt, created_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: SetTaskSqid :exec
UPDATE task SET sqid = ? WHERE id = ?;

-- name: GetTaskBySqid :one
SELECT * FROM task WHERE sqid = ?;

-- name: SubtreeOrdered :many
-- Every task under (and including) root, depth-first in rank order.
WITH RECURSIVE sub (id, depth, path) AS (
    SELECT t.id, 0, t.rank FROM task t WHERE t.id = sqlc.arg(root_id)
    UNION ALL
    SELECT c.id, sub.depth + 1, sub.path || ' ' || c.rank
    FROM task c JOIN sub ON c.parent_id = sub.id
)
SELECT task.id, task.sqid, task.parent_id, task.title, task.state, task.rank,
       CAST(sub.depth AS INTEGER) AS depth, CAST(sub.path AS TEXT) AS path
FROM sub JOIN task ON task.id = sub.id
ORDER BY sub.path;

-- name: NextReadyLeaf :one
-- First open leaf under root (rank order, depth-first) whose precursors are satisfied:
-- merged (done), or completed earlier in this same run (in_review on the same branch).
WITH RECURSIVE sub (id, path) AS (
    SELECT t.id, t.rank FROM task t WHERE t.id = sqlc.arg(root_id)
    UNION ALL
    SELECT c.id, sub.path || ' ' || c.rank FROM task c JOIN sub ON c.parent_id = sub.id
)
SELECT task.id, task.sqid, task.title, task.prompt
FROM sub JOIN task ON task.id = sub.id
WHERE task.state IN ('open', 'in_progress')
  AND NOT EXISTS (SELECT 1 FROM task k WHERE k.parent_id = task.id AND k.state <> 'cancelled')
  AND NOT EXISTS (
      SELECT 1 FROM task_dependency d JOIN task p ON p.id = d.depends_on_id
      WHERE d.task_id = task.id
        AND NOT (p.state = 'done'
                 OR (p.state = 'in_review' AND p.completed_run_id = sqlc.arg(run_id))))
ORDER BY sub.path
LIMIT 1;

-- name: IsAncestorOrSelf :one
-- 1 if sqlc.arg(ancestor_id) is node_id or one of its ancestors.
WITH RECURSIVE up (id, parent_id) AS (
    SELECT t.id, t.parent_id FROM task t WHERE t.id = sqlc.arg(node_id)
    UNION ALL
    SELECT t.id, t.parent_id FROM task t JOIN up ON t.id = up.parent_id
)
SELECT CAST(COUNT(*) > 0 AS INTEGER) AS hit FROM up WHERE up.id = CAST(sqlc.arg(ancestor_id) AS INTEGER);

-- name: PrecursorReaches :one
-- 1 if from_id transitively depends on to_id (adding to_id -> from_id would cycle).
WITH RECURSIVE reach (id) AS (
    SELECT d.depends_on_id FROM task_dependency d WHERE d.task_id = sqlc.arg(from_id)
    UNION
    SELECT d.depends_on_id FROM task_dependency d JOIN reach ON d.task_id = reach.id
)
SELECT CAST(COUNT(*) > 0 AS INTEGER) AS hit FROM reach WHERE reach.id = CAST(sqlc.arg(to_id) AS INTEGER);

-- name: SubtreeLocked :one
-- 1 if an active run's root is an ancestor-or-self or a descendant of root_id.
WITH RECURSIVE up (id, parent_id) AS (
    SELECT t.id, t.parent_id FROM task t WHERE t.id = sqlc.arg(root_id)
    UNION ALL
    SELECT t.id, t.parent_id FROM task t JOIN up ON t.id = up.parent_id
), down (id) AS (
    SELECT t.id FROM task t WHERE t.parent_id = sqlc.arg(root_id)
    UNION ALL
    SELECT t.id FROM task t JOIN down ON t.parent_id = down.id
), near (id) AS (
    SELECT id FROM up UNION SELECT id FROM down
)
SELECT CAST(COUNT(*) > 0 AS INTEGER) AS locked
FROM run r JOIN near ON near.id = r.task_id
WHERE r.state NOT IN ('succeeded', 'failed', 'cancelled');

-- name: AddPrecursor :exec
INSERT INTO task_dependency (task_id, depends_on_id, created_by) VALUES (?, ?, ?);

-- name: CompleteTask :exec
-- Agent marks a leaf complete: in_review until its PR merges.
UPDATE task SET state = 'in_review', completion_summary = ?, completed_run_id = ?, completed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ?;

-- name: MarkRunTasksDone :exec
-- PR merged: everything this run completed becomes done.
UPDATE task SET state = 'done', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE completed_run_id = ? AND state = 'in_review';
```

  Also add (straightforward, unverified): `CreateTemplate`, `GetTemplateByName`, `ListTemplates`,
  `GetProjectBySlug`, `CreateRunner`, `ListRunners`, `UpdateRunnerState`, `InsertLogLine`,
  `CreateQuestion`, `ListOpenQuestions`, `UpsertPushSubscription`, `GetMeta`, `SetMeta`,
  `ListChildren` (by parent, rank order), `SiblingRanks` (neighbour ranks for positioning),
  `InsertTaskEvent`.

- [ ] **Step 4: failing tests** — port the two scratch tests:
  - `db_test.go`: template → project → repo → task → run → two log lines; `ListActiveRuns` = 1;
    `SearchLogs("disk")` hits exactly the matching line; FK violation errors; bad CHECK value
    errors; `Migrate(down)` succeeds.
  - `tree_test.go`: build `epic{c1{c1a,c1b}, c2, c3}` with ranks `V, VV, W` (c1=V, c2=VV, c3=W; c1a=V, c1b=W), assign sqids
    via `sqids-go` in the same tx. Assert:
    - `SubtreeOrdered` = `epic,c1,c1a,c1b,c2,c3`
    - with `c1a needs c3`: `NextReadyLeaf(root, runA)` = `c1b`
    - `PrecursorReaches(c1a→c3)` = 1; `IsAncestorOrSelf(c1a, root)` = 1
    - complete `c1b,c2,c3` in runA → `NextReadyLeaf(root, runA)` = `c1a`, but
      `NextReadyLeaf(c1, run99)` = `sql.ErrNoRows`; after `MarkRunTasksDone(runA)` run99 gets `c1a`
    - a running run on `c1` locks `root`, `c1`, `c1a`, not `c2`
    - rejected: sqid update, duplicate sibling rank, duplicate root rank, cross-project parent,
      cross-project precursor
    - deleting `epic` cascades the whole subtree
- [ ] **Step 5:** `task sqlc`; implement `db.go`; `CGO_ENABLED=0 go test ./cmd/wrangl/internal/db/` green.

### Task 5: `stack` primitives (pure, no DB)

**Files:** `cmd/wrangl/internal/stack/{rank,sqid}.go` + tests

**Interfaces:**
```go
// Ranks are base-62 strings over "0-9A-Za-z" that never end in '0', so a key strictly
// between any two distinct keys always exists. "" as a bound means unbounded.
func RankBetween(lo, hi string) (string, error) // error if lo >= hi (non-empty)
func RankFirst() string                         // "V" (middle of the alphabet)
func RankAfter(lo string) string                // RankBetween(lo, "")
func RankBefore(hi string) string               // RankBetween("", hi)

type Sqids struct{ /* wraps sqids.Sqids */ }
func NewSqids(alphabet string) (*Sqids, error)  // MinLength 6
func ShuffledAlphabet(r io.Reader) string       // crypto/rand Fisher–Yates of sqids default alphabet; used once at init
func (s *Sqids) Encode(id int64) string
func (s *Sqids) Decode(sqid string) (int64, error) // rejects non-canonical encodings
```

- [ ] **Step 1: failing tests** — `RankBetween` table cases (`("", "") → mid`, adjacent digits, `("a", "a0V")`, long prefixes); property test: 10 000 random inserts at random positions in a slice keep it strictly sorted under Go string comparison and no key ends in `'0'`; repeated appends stay ≤ 12 chars for 1 000 appends (amortised growth check). Sqids: round-trip, `Decode` rejects a re-encoded-differently string, two alphabets produce different sqids for the same id.
- [ ] **Step 2:** Implement (midpoint digit-by-digit; pad shorter key conceptually with `'0'` on the low side and `'z'+1` on the high side).

### Task 6: `controller init`, `migrate`, `secret`

**Files:** `cmd/wrangl/internal/controller/initcmd.go`, `secrets.go`; modify `cli/controller.go`

- [ ] **Step 1: failing tests**
  - `Init(dataDir, configPath)` creates `data_dir` (0750) with `bin/`, `nats/`, `secrets/` (0700); writes a config skeleton only if absent; creates `wrangl.db` with migrations; stores `meta.sqids_alphabet` once (second run leaves it unchanged); idempotent.
  - `SecretSet(name, r)` writes `secrets/<name>` 0600 atomically (temp + rename); names restricted to `[a-z0-9_.-]+`; `SecretList` returns names only; `SecretRm` deletes.
- [ ] **Step 2:** Implement; `controller migrate [up|down|status]` → `db.Migrate`. Leave `// M2:` (NATS operator/TLS) and `// M8:` (VAPID) markers in `Init`.

### Task 7: `controller serve` skeleton

**Files:** `cmd/wrangl/internal/controller/server.go`, `server_test.go`

**Interfaces:**
```go
func New(cfg *config.Controller) (*Server, error)
// Run: open DB; embedded NATS (JetStream, StoreDir data_dir/nats/js, listen cfg.Listen.NATS);
// in-process client (nats.InProcessServer); ensure streams WR_EVT (wr.evt.>, file, MaxAge 7d)
// and WR_PTY (wr.pty.>, file, MaxBytes 2GiB, MaxAge 3d); HTTP on cfg.Listen.HTTP and
// cfg.Listen.Unix (0660); block until ctx done; shut down HTTP → NATS → DB.
func (s *Server) Run(ctx context.Context) error
```

- [ ] **Step 1: failing test** — temp data dir, random ports, socket in temp dir: `GET /healthz` via TCP and unix socket → `200 {"db":"ok","nats":"ok","jetstream":"ok"}`; both streams exist; cancel → returns nil within 5 s, socket removed.
- [ ] **Step 2:** Implement; wire `wrangl controller serve`. Manual: `task build-wrangl && build/wrangl controller init --data-dir /tmp/wr --config /tmp/wr/c.yaml && build/wrangl controller serve --config /tmp/wr/c.yaml`, then `curl --unix-socket /tmp/wr/api.sock http://x/healthz`.

### Task 8: Wrap-up

- [ ] `go vet ./... && go test ./... && task sqlc-check` green.
- [ ] README: short `wrangl` section (`task build-wrangl`, link to spec). Mark M1 done in spec §15.

## Out of scope for M1

NATS auth/enrolment, AWS/GitHub calls, work-stack write rules beyond the DB constraints
(M4 `stack` service), HTTP API beyond `/healthz`, TUI, web.

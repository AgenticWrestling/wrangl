-- +goose Up
-- A new question kind: a queued run whose task resolves to no repo waits for the owner to pick one
-- (ADR 0012 section 4). Widened in place like 00040. One open item per run.
PRAGMA writable_schema = ON;
UPDATE sqlite_master
   SET sql = replace(sql, '''unplaceable'')', '''unplaceable'', ''needs_repo'')')
 WHERE type = 'table' AND name = 'question';
PRAGMA writable_schema = OFF;

CREATE TABLE _schema_bump (x);
DROP TABLE _schema_bump;

CREATE UNIQUE INDEX question_needs_repo_open ON question (run_id) WHERE kind = 'needs_repo' AND state = 'open';

-- +goose Down
DROP INDEX question_needs_repo_open;

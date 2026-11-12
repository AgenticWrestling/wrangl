-- +goose Up
-- A new question kind: a queued run that only the owner can get placed (ADR 0009 section 9).
-- Widened in place like 00036. One open item per run.
PRAGMA writable_schema = ON;
UPDATE sqlite_master
   SET sql = replace(sql, '''tool_error'')', '''tool_error'', ''unplaceable'')')
 WHERE type = 'table' AND name = 'question';
PRAGMA writable_schema = OFF;

CREATE TABLE _schema_bump (x);
DROP TABLE _schema_bump;

CREATE UNIQUE INDEX question_unplaceable_open ON question (run_id) WHERE kind = 'unplaceable' AND state = 'open';

-- +goose Down
DROP INDEX question_unplaceable_open;

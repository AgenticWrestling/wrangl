-- +goose Up
-- A new question kind: a tool call the controller ran for a runner failed (GitHub unreachable or
-- refused, say). It is a notice in the Inbox, raised when it happens. Widened in place like 00030.
PRAGMA writable_schema = ON;
UPDATE sqlite_master
   SET sql = replace(sql, '''task_review'')', '''task_review'', ''tool_error'')')
 WHERE type = 'table' AND name = 'question';
PRAGMA writable_schema = OFF;

CREATE TABLE _schema_bump (x);
DROP TABLE _schema_bump;

-- +goose Down
SELECT 1;

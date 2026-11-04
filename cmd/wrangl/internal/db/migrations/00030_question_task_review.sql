-- +goose Up
-- A new question kind: a task needs the owner's call without a pull request to review
-- (in review with no tracked PR, or its run failed or was blocked). Widened in place like
-- 00012; the throwaway table moves the schema cookie.
PRAGMA writable_schema = ON;
UPDATE sqlite_master
   SET sql = replace(sql, '''deliverable_review'')', '''deliverable_review'', ''task_review'')')
 WHERE type = 'table' AND name = 'question';
PRAGMA writable_schema = OFF;

CREATE TABLE _schema_bump (x);
DROP TABLE _schema_bump;

-- +goose Down
SELECT 1;

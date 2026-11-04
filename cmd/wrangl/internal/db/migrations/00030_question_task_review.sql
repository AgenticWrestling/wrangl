-- +goose Up
-- A new question kind: the owner accepts or reopens a task the agent completed with no
-- tracked pull request. Widened in place like 00012; the throwaway table moves the schema cookie.
PRAGMA writable_schema = ON;
UPDATE sqlite_master
   SET sql = replace(sql, '''deliverable_review'')', '''deliverable_review'', ''task_review'')')
 WHERE type = 'table' AND name = 'question';
PRAGMA writable_schema = OFF;

CREATE TABLE _schema_bump (x);
DROP TABLE _schema_bump;

-- +goose Down
SELECT 1;

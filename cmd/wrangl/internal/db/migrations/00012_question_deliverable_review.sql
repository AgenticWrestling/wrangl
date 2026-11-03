-- +goose Up
-- A new question kind: the owner reviews a run's deliverables (no pull request). Widened in
-- place like 00009/00010; the throwaway table moves the schema cookie.
PRAGMA writable_schema = ON;
UPDATE sqlite_master
   SET sql = replace(sql, '''plan_review'', ''system'')', '''plan_review'', ''system'', ''deliverable_review'')')
 WHERE type = 'table' AND name = 'question';
PRAGMA writable_schema = OFF;

CREATE TABLE _schema_bump (x);
DROP TABLE _schema_bump;

-- +goose Down
SELECT 1;

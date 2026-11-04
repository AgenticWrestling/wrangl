-- +goose Up
-- A new run kind: Smart Merge ("resolve") merges a PR's base branch into its branch and
-- resolves the conflicts. Widened in place like 00030; the throwaway table moves the
-- schema cookie.
PRAGMA writable_schema = ON;
UPDATE sqlite_master
   SET sql = replace(sql, '''revise'', ''plan'')', '''revise'', ''plan'', ''resolve'')')
 WHERE type = 'table' AND name = 'run';
PRAGMA writable_schema = OFF;

CREATE TABLE _schema_bump (x);
DROP TABLE _schema_bump;

-- +goose Down
SELECT 1;

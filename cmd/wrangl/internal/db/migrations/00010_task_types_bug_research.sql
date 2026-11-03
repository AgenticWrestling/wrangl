-- +goose Up
-- Task types: add bug and research; spec is folded into docs (the CHECK keeps accepting
-- 'spec' so nothing old breaks, but no row uses it any more). Same in-place CHECK widening as
-- 00009, followed by a throwaway table so the schema cookie moves and connections reload.
UPDATE task SET task_type = 'docs' WHERE task_type = 'spec';

PRAGMA writable_schema = ON;
UPDATE sqlite_master
   SET sql = replace(sql, '''milestone'', ''feature'')', '''milestone'', ''feature'', ''bug'', ''research'')')
 WHERE type = 'table' AND name = 'task';
PRAGMA writable_schema = OFF;

CREATE TABLE _schema_bump (x);
DROP TABLE _schema_bump;

-- +goose Down
SELECT 1;

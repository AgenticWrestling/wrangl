-- +goose Up
-- Planning task types (release, milestone, feature) and a target date.
--
-- SQLite cannot alter a CHECK constraint, and rebuilding `task` would cascade-delete its
-- children (ON DELETE CASCADE with foreign keys on), so the allowed list is widened in
-- place by rewriting the stored CREATE TABLE text, as the SQLite ALTER TABLE documentation
-- describes for relaxing constraints. ADD COLUMN must come first: it splices into the stored
-- text at offsets from the parsed schema, which the edit would invalidate. Connections other
-- than the migrating one re-read the schema when this commits.
-- Target date: YYYY-MM-DD, NULL = none.
ALTER TABLE task ADD COLUMN target_date TEXT;

PRAGMA writable_schema = ON;
UPDATE sqlite_master
   SET sql = replace(sql, '''docs'', ''review'')', '''docs'', ''review'', ''release'', ''milestone'', ''feature'')')
 WHERE type = 'table' AND name = 'task';
PRAGMA writable_schema = OFF;

-- +goose Down
ALTER TABLE task DROP COLUMN target_date;

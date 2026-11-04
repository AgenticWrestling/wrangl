-- +goose Up
-- Agent issue tools (ADR 0004): GitHub-side metadata for synced issues, full-text search,
-- and a lazily filled comment cache. Kept beside the issue table so existing queries are unchanged.
CREATE TABLE issue_meta (
    issue_id      INTEGER PRIMARY KEY REFERENCES issue (id) ON DELETE CASCADE,
    gh_updated_at TEXT    NOT NULL DEFAULT '',  -- GitHub's updated_at, normalised to UTC RFC3339
    author        TEXT    NOT NULL DEFAULT '',
    labels_json   TEXT    NOT NULL DEFAULT '[]',
    comment_count INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE issue_comment (
    id         INTEGER PRIMARY KEY,
    issue_id   INTEGER NOT NULL REFERENCES issue (id) ON DELETE CASCADE,
    gh_id      INTEGER NOT NULL,
    author     TEXT    NOT NULL DEFAULT '',
    body       TEXT    NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL DEFAULT '',
    UNIQUE (issue_id, gh_id)
);

CREATE VIRTUAL TABLE issue_fts USING fts5 (
    title, body, content = 'issue', content_rowid = 'id', tokenize = 'porter unicode61'
);
-- +goose StatementBegin
CREATE TRIGGER issue_ai AFTER INSERT ON issue BEGIN
    INSERT INTO issue_fts (rowid, title, body) VALUES (new.id, new.title, new.body);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER issue_ad AFTER DELETE ON issue BEGIN
    INSERT INTO issue_fts (issue_fts, rowid, title, body) VALUES ('delete', old.id, old.title, old.body);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER issue_au AFTER UPDATE ON issue BEGIN
    INSERT INTO issue_fts (issue_fts, rowid, title, body) VALUES ('delete', old.id, old.title, old.body);
    INSERT INTO issue_fts (rowid, title, body) VALUES (new.id, new.title, new.body);
END;
-- +goose StatementEnd
INSERT INTO issue_fts (issue_fts) VALUES ('rebuild');

-- +goose Down
DROP TRIGGER issue_au;
DROP TRIGGER issue_ad;
DROP TRIGGER issue_ai;
DROP TABLE issue_fts;
DROP TABLE issue_comment;
DROP TABLE issue_meta;

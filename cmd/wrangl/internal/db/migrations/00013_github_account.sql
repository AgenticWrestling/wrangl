-- +goose Up
-- GitHub accounts wrangl works with: the owner's personal account and any organisations they
-- link in Settings. A GitHub App has to be created in the account that owns the repositories
-- (personal or org), so each account gets its own author App; app_id is NULL until it exists.
CREATE TABLE github_account (
    id         INTEGER PRIMARY KEY,
    login      TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    kind       TEXT    NOT NULL CHECK (kind IN ('user', 'org')),
    app_id     INTEGER,
    slug       TEXT    NOT NULL DEFAULT '',
    html_url   TEXT    NOT NULL DEFAULT '',
    key_file   TEXT    NOT NULL DEFAULT '',  -- relative to the data dir
    created_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- +goose Down
DROP TABLE github_account;

-- name: AddLogEntry :exec
INSERT INTO log_entry (at, kind, level, subject, title, detail) VALUES (?, ?, ?, ?, ?, ?);

-- name: ListLogEntries :many
SELECT * FROM log_entry ORDER BY at DESC, id DESC LIMIT sqlc.arg(lim);

-- name: PruneLogEntries :exec
DELETE FROM log_entry WHERE at < ?;

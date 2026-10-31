-- name: SearchLogs :many
SELECT l.id, l.run_id, l.ts, l.stream, l.message
FROM log_line_fts f
JOIN log_line l ON l.id = f.rowid
WHERE f.message MATCH ?
ORDER BY l.id DESC
LIMIT ?;

-- name: InsertLogLine :exec
INSERT INTO log_line (run_id, seq, ts, stream, level, message)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (run_id, seq) DO NOTHING;

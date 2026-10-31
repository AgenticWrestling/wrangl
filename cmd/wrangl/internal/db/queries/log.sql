-- name: SearchLogs :many
SELECT l.id, l.run_id, l.ts, l.stream, l.message
FROM log_line_fts f
JOIN log_line l ON l.id = f.rowid
WHERE f.message MATCH ?
ORDER BY l.id DESC
LIMIT ?;

-- name: SearchLogsFiltered :many
SELECT l.id, l.run_id, l.ts, l.stream, l.message,
       CAST(p.slug AS TEXT) AS project_slug, CAST(COALESCE(rn.name, '') AS TEXT) AS runner_name,
       CAST(COALESCE(t.sqid, '') AS TEXT) AS task_sqid
FROM log_line_fts f
JOIN log_line l ON l.id = f.rowid
JOIN run r ON r.id = l.run_id
JOIN task t ON t.id = r.task_id
JOIN project p ON p.id = t.project_id
LEFT JOIN runner rn ON rn.id = r.runner_id
WHERE f.message MATCH sqlc.arg(query)
  AND (CAST(sqlc.arg(project) AS TEXT) = '' OR p.slug = CAST(sqlc.arg(project) AS TEXT))
  AND (CAST(sqlc.arg(runner) AS TEXT) = '' OR rn.name = CAST(sqlc.arg(runner) AS TEXT))
  AND (CAST(sqlc.arg(since) AS TEXT) = '' OR l.ts >= CAST(sqlc.arg(since) AS TEXT))
ORDER BY l.id DESC
LIMIT sqlc.arg(lim);

-- name: InsertLogLine :exec
INSERT INTO log_line (run_id, seq, ts, stream, level, message)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (run_id, seq) DO NOTHING;

-- name: RunLogs :many
SELECT * FROM log_line
WHERE run_id = sqlc.arg(run_id) AND id > sqlc.arg(after_id)
  AND (CAST(sqlc.arg(stream) AS TEXT) = '' OR stream = CAST(sqlc.arg(stream) AS TEXT))
  AND (CAST(sqlc.arg(since) AS TEXT) = '' OR ts >= CAST(sqlc.arg(since) AS TEXT))
ORDER BY id
LIMIT sqlc.arg(lim);

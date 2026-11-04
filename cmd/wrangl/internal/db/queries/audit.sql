-- name: GetProjectCitationAudit :one
SELECT citation_audit FROM project WHERE id = ?;

-- name: SetProjectCitationAudit :exec
UPDATE project SET citation_audit = ? WHERE id = ?;

-- name: GetPRAudit :one
SELECT audit_sha, audit_json FROM pull_request WHERE id = ?;

-- name: SetPRAudit :exec
UPDATE pull_request SET audit_sha = ?, audit_json = ? WHERE id = ?;

-- name: GetProjectAutoResolve :one
SELECT auto_resolve FROM project WHERE id = ?;

-- name: SetProjectAutoResolve :exec
UPDATE project SET auto_resolve = ? WHERE id = ?;

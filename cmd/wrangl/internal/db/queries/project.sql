-- name: CreateProject :one
INSERT INTO project (slug, name, description, default_template_id)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: ListProjects :many
-- archived: 'active' (default for every caller), 'archived' or 'all'.
SELECT * FROM project
WHERE CAST(sqlc.arg(archived) AS TEXT) = 'all'
   OR (CAST(sqlc.arg(archived) AS TEXT) = 'archived' AND archived_at IS NOT NULL)
   OR (CAST(sqlc.arg(archived) AS TEXT) NOT IN ('all', 'archived') AND archived_at IS NULL)
ORDER BY slug;

-- name: ArchiveProject :one
UPDATE project SET archived_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND archived_at IS NULL
RETURNING *;

-- name: RestoreProject :one
UPDATE project SET archived_at = NULL, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = ? AND archived_at IS NOT NULL
RETURNING *;

-- name: GetProjectBySlug :one
SELECT * FROM project WHERE slug = ?;

-- name: GetProject :one
SELECT * FROM project WHERE id = ?;

-- name: UpdateProject :one
UPDATE project
SET name = sqlc.arg(name), description = sqlc.arg(description),
    default_template_id = sqlc.narg(default_template_id), plan_template_id = sqlc.narg(plan_template_id),
    auto_merge = sqlc.arg(auto_merge), auto_queue = sqlc.arg(auto_queue), priority = sqlc.arg(priority),
    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DeleteProject :execrows
DELETE FROM project WHERE id = ?;

-- name: CreateRepo :one
INSERT INTO repo (project_id, owner, name, default_branch, gh_installation_id, clone_url)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: ListRepos :many
SELECT sqlc.embed(r), CAST(p.slug AS TEXT) AS project_slug
FROM repo r JOIN project p ON p.id = r.project_id
WHERE sqlc.arg(project_id) = 0 OR r.project_id = sqlc.arg(project_id)
ORDER BY p.slug, r.owner, r.name;

-- name: GetRepo :one
SELECT * FROM repo WHERE id = ?;

-- name: GetProjectRepo :one
SELECT * FROM repo WHERE project_id = ? AND owner = ? AND name = ?;

-- name: DeleteRepo :execrows
DELETE FROM repo WHERE id = ?;

-- name: UpsertIssue :one
INSERT INTO issue (repo_id, gh_number, title, body, state, url, synced_at)
VALUES (sqlc.arg(repo_id), sqlc.arg(gh_number), sqlc.arg(title), sqlc.arg(body), sqlc.arg(state), sqlc.arg(url), sqlc.arg(synced_at))
ON CONFLICT (repo_id, gh_number) DO UPDATE
SET title = excluded.title, body = excluded.body, state = excluded.state, url = excluded.url, synced_at = excluded.synced_at
RETURNING *;

-- name: ListIssues :many
-- Filter params are CAST so sqlc types them; untyped (interface{}) params left unset
-- become NULL and silently match nothing (quirk 7).
SELECT sqlc.embed(i), CAST(r.owner AS TEXT) AS repo_owner, CAST(r.name AS TEXT) AS repo_name,
       CAST(COALESCE((SELECT t.sqid FROM task t WHERE t.issue_id = i.id LIMIT 1), '') AS TEXT) AS task_sqid
FROM issue i JOIN repo r ON r.id = i.repo_id
WHERE (CAST(sqlc.arg(project_id) AS INTEGER) = 0 OR r.project_id = CAST(sqlc.arg(project_id) AS INTEGER))
  AND (CAST(sqlc.arg(state) AS TEXT) = 'all' OR i.state = CAST(sqlc.arg(state) AS TEXT))
ORDER BY r.owner, r.name, i.gh_number;

-- name: GetIssueByNumber :one
SELECT i.* FROM issue i JOIN repo r ON r.id = i.repo_id
WHERE r.project_id = sqlc.arg(project_id) AND r.owner = sqlc.arg(owner) AND r.name = sqlc.arg(name)
  AND i.gh_number = sqlc.arg(number);

-- name: GetIssue :one
SELECT * FROM issue WHERE id = ?;

-- name: GetNotesRepoID :one
SELECT repo_id FROM project_notes_repo WHERE project_id = ?;

-- name: SetNotesRepo :exec
INSERT INTO project_notes_repo (project_id, repo_id) VALUES (?, ?)
ON CONFLICT (project_id) DO UPDATE SET repo_id = excluded.repo_id;

-- name: ClearNotesRepo :exec
DELETE FROM project_notes_repo WHERE project_id = ?;

-- name: SetProjectDefaultRepo :exec
UPDATE project SET default_repo_id = sqlc.narg(default_repo_id), updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE id = sqlc.arg(id);

-- name: SetRepoNote :exec
UPDATE repo SET note = sqlc.arg(note) WHERE id = sqlc.arg(id);

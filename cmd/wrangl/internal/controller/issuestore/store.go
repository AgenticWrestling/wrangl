// Package issuestore reads and writes the synced issue table together with its GitHub-side
// metadata (issue_meta), full-text index (issue_fts) and comment cache (issue_comment).
// Every writer (sync, webhook, live reads) goes through Save so the same
// "only newer wins" rule applies everywhere (ADR 0004).
package issuestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
)

// TimeFormat is the one format synced_at is written in.
const TimeFormat = "2006-01-02T15:04:05.000Z"

// NormTime turns a GitHub timestamp into UTC RFC3339 so strings compare chronologically.
// Unparseable or empty input yields "".
func NormTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// ParseTime parses either timestamp format used for synced_at.
func ParseTime(s string) (time.Time, bool) {
	for _, f := range []string{TimeFormat, time.RFC3339} {
		if t, err := time.Parse(f, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Row is a synced issue with its metadata.
type Row struct {
	ID           int64
	RepoID       int64
	Owner, Repo  string
	Number       int64
	Title, Body  string
	State, URL   string
	SyncedAt     string
	GhUpdatedAt  string
	Author       string
	Labels       []string
	CommentCount int64
	TaskSqid     string
}

// Save stores an issue only if it is not older than what we hold. It returns the issue id and
// whether the content was applied. An equal or older updated_at (our own write echoing back,
// a duplicate or reordered webhook) only refreshes synced_at when equal. An issue with no
// UpdatedAt (a source without metadata) is always applied.
func Save(ctx context.Context, d *db.DB, repoID int64, i gh.Issue, now time.Time) (int64, bool, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	synced := now.UTC().Format(TimeFormat)
	incoming := NormTime(i.UpdatedAt)

	var id int64
	var have string
	err = tx.QueryRowContext(ctx, `SELECT i.id, COALESCE(m.gh_updated_at, '') FROM issue i
		LEFT JOIN issue_meta m ON m.issue_id = i.id WHERE i.repo_id = ? AND i.gh_number = ?`, repoID, i.Number).Scan(&id, &have)
	exists := !errors.Is(err, sql.ErrNoRows)
	if err != nil && exists {
		return 0, false, err
	}
	if exists && incoming != "" && have != "" && incoming <= have {
		if incoming == have {
			if _, err := tx.ExecContext(ctx, `UPDATE issue SET synced_at = ? WHERE id = ?`, synced, id); err != nil {
				return 0, false, err
			}
		}
		return id, false, tx.Commit()
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO issue (repo_id, gh_number, title, body, state, url, synced_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo_id, gh_number) DO UPDATE
		SET title = excluded.title, body = excluded.body, state = excluded.state, url = excluded.url, synced_at = excluded.synced_at
		RETURNING id`, repoID, i.Number, i.Title, i.Body, i.State, i.URL, synced).Scan(&id); err != nil {
		return 0, false, err
	}
	labels, _ := json.Marshal(append([]string{}, i.Labels...))
	if _, err := tx.ExecContext(ctx, `INSERT INTO issue_meta (issue_id, gh_updated_at, author, labels_json, comment_count)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (issue_id) DO UPDATE
		SET gh_updated_at = excluded.gh_updated_at, author = excluded.author, labels_json = excluded.labels_json, comment_count = excluded.comment_count`,
		id, incoming, i.Author, string(labels), i.Comments); err != nil {
		return 0, false, err
	}
	return id, true, tx.Commit()
}

const selectRow = `SELECT i.id, i.repo_id, r.owner, r.name, i.gh_number, i.title, i.body, i.state, i.url, i.synced_at,
	COALESCE(m.gh_updated_at, ''), COALESCE(m.author, ''), COALESCE(m.labels_json, '[]'), COALESCE(m.comment_count, 0),
	COALESCE((SELECT t.sqid FROM task t WHERE t.issue_id = i.id LIMIT 1), '')
	FROM issue i JOIN repo r ON r.id = i.repo_id LEFT JOIN issue_meta m ON m.issue_id = i.id `

type scanner interface{ Scan(...any) error }

func scanRow(s scanner) (Row, error) {
	var r Row
	var labels string
	if err := s.Scan(&r.ID, &r.RepoID, &r.Owner, &r.Repo, &r.Number, &r.Title, &r.Body, &r.State, &r.URL, &r.SyncedAt,
		&r.GhUpdatedAt, &r.Author, &labels, &r.CommentCount, &r.TaskSqid); err != nil {
		return r, err
	}
	_ = json.Unmarshal([]byte(labels), &r.Labels)
	return r, nil
}

// Get returns one issue; sql.ErrNoRows when we do not hold it.
func Get(ctx context.Context, d *db.DB, repoID, number int64) (Row, error) {
	return scanRow(d.R.QueryRowContext(ctx, selectRow+`WHERE i.repo_id = ? AND i.gh_number = ?`, repoID, number))
}

// Count is how many issues are synced for a repo.
func Count(ctx context.Context, d *db.DB, repoID int64) (int, error) {
	var n int
	err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue WHERE repo_id = ?`, repoID).Scan(&n)
	return n, err
}

var nonWord = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// FTSQuery turns free text into a safe FTS5 query: each word is quoted, so operators,
// quotes, NEAR(), column filters and * in the input are plain text. "" means nothing to search.
func FTSQuery(text string) string {
	var words []string
	for _, w := range nonWord.Split(text, -1) {
		if w != "" {
			words = append(words, `"`+w+`"`)
		}
	}
	return strings.Join(words, " ")
}

// SearchHit is a Row plus a snippet of the matching text.
type SearchHit struct {
	Row
	Snippet string
}

// Search runs an FTS query over the issues of one project (repoID 0 = all its repos).
// state is open, closed or all. match must come from FTSQuery.
func Search(ctx context.Context, d *db.DB, projectID, repoID int64, match, state string, limit int) ([]SearchHit, error) {
	q := strings.Replace(selectRow, "SELECT i.id,", "SELECT snippet(issue_fts, -1, '', '', ' … ', 24), i.id,", 1)
	q = strings.Replace(q, "FROM issue i", "FROM issue_fts JOIN issue i ON i.id = issue_fts.rowid", 1)
	rows, err := d.R.QueryContext(ctx, q+`WHERE issue_fts MATCH ? AND r.project_id = ? AND (? = 0 OR r.id = ?)
		AND (? = 'all' OR i.state = ?) ORDER BY bm25(issue_fts), i.id LIMIT ?`,
		match, projectID, repoID, repoID, state, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		var labels string
		if err := rows.Scan(&h.Snippet, &h.ID, &h.RepoID, &h.Owner, &h.Repo, &h.Number, &h.Title, &h.Body, &h.State, &h.URL, &h.SyncedAt,
			&h.GhUpdatedAt, &h.Author, &labels, &h.CommentCount, &h.TaskSqid); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(labels), &h.Labels)
		out = append(out, h)
	}
	return out, rows.Err()
}

// Comment is a cached issue comment.
type Comment struct {
	GhID               int64
	Author, Body, Time string
}

// CachedComments returns up to limit+1 comments after a GitHub comment id (0 = from the start).
func CachedComments(ctx context.Context, d *db.DB, issueID, after int64, limit int) ([]Comment, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT gh_id, author, body, created_at FROM issue_comment
		WHERE issue_id = ? AND gh_id > ? ORDER BY gh_id LIMIT ?`, issueID, after, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.GhID, &c.Author, &c.Body, &c.Time); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CommentCount is how many comments are cached for an issue.
func CommentCount(ctx context.Context, d *db.DB, issueID int64) (int64, error) {
	var n int64
	err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_comment WHERE issue_id = ?`, issueID).Scan(&n)
	return n, err
}

// ReplaceComments swaps the cached comments of an issue for a fresh complete list and, when
// complete, records the count so the cache is not refetched until the issue's count changes.
func ReplaceComments(ctx context.Context, d *db.DB, issueID int64, cs []gh.IssueComment, complete bool) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM issue_comment WHERE issue_id = ?`, issueID); err != nil {
		return err
	}
	for _, c := range cs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO issue_comment (issue_id, gh_id, author, body, created_at) VALUES (?, ?, ?, ?, ?)`,
			issueID, c.ID, c.Author, c.Body, c.CreatedAt); err != nil {
			return err
		}
	}
	if complete {
		if _, err := tx.ExecContext(ctx, `UPDATE issue_meta SET comment_count = ? WHERE issue_id = ?`, len(cs), issueID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

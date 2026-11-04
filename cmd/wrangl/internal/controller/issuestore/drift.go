package issuestore

import (
	"context"
	"slices"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
)

// ListRepo returns every synced issue of a repo, by number.
func ListRepo(ctx context.Context, d *db.DB, repoID int64) ([]Row, error) {
	rows, err := d.R.QueryContext(ctx, selectRow+`WHERE i.repo_id = ? ORDER BY i.gh_number`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Delete removes a synced issue (deleted or transferred on GitHub). Tasks that pointed at it
// keep existing with no link (task.issue_id is ON DELETE SET NULL). It reports whether a row existed.
func Delete(ctx context.Context, d *db.DB, repoID, number int64) (bool, error) {
	res, err := d.W.ExecContext(ctx, `DELETE FROM issue WHERE repo_id = ? AND gh_number = ?`, repoID, number)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Drift kinds.
const (
	DriftMissingLocal  = "missing_local"  // on GitHub, not in the table (a missed webhook)
	DriftMissingRemote = "missing_remote" // in the table, gone from GitHub (deleted or transferred)
	DriftStale         = "stale"          // GitHub has a newer version
	DriftDiffers       = "differs"        // same (or older) updated_at but different content
)

// Drift is one issue whose local row differs from GitHub.
type Drift struct {
	Ref    string   `json:"ref"` // owner/name#n
	Kind   string   `json:"kind"`
	Fields []string `json:"fields,omitempty"` // which of title, body, state, labels differ
	Local  string   `json:"local_updated_at,omitempty"`
	Remote string   `json:"remote_updated_at,omitempty"`
}

// Compare lists the differences between a repo's synced rows and GitHub's issues (all states).
// Only the content we serve is compared: title, body, state and labels (label order ignored).
func Compare(owner, repo string, local []Row, remote []gh.Issue) []Drift {
	ref := func(n int64) string { return owner + "/" + repo + "#" + itoa(n) }
	have := map[int64]Row{}
	for _, r := range local {
		have[r.Number] = r
	}
	var out []Drift
	seen := map[int64]bool{}
	for _, i := range remote {
		seen[i.Number] = true
		l, ok := have[i.Number]
		if !ok {
			out = append(out, Drift{Ref: ref(i.Number), Kind: DriftMissingLocal, Remote: NormTime(i.UpdatedAt)})
			continue
		}
		var f []string
		if l.Title != i.Title {
			f = append(f, "title")
		}
		if l.Body != i.Body {
			f = append(f, "body")
		}
		if l.State != i.State {
			f = append(f, "state")
		}
		if !sameLabels(l.Labels, i.Labels) {
			f = append(f, "labels")
		}
		rem := NormTime(i.UpdatedAt)
		switch {
		case rem != "" && l.GhUpdatedAt != "" && rem > l.GhUpdatedAt:
			out = append(out, Drift{Ref: ref(i.Number), Kind: DriftStale, Fields: f, Local: l.GhUpdatedAt, Remote: rem})
		case len(f) > 0:
			out = append(out, Drift{Ref: ref(i.Number), Kind: DriftDiffers, Fields: f, Local: l.GhUpdatedAt, Remote: rem})
		}
	}
	for _, l := range local {
		if !seen[l.Number] {
			out = append(out, Drift{Ref: ref(l.Number), Kind: DriftMissingRemote, Local: l.GhUpdatedAt})
		}
	}
	return out
}

func itoa(n int64) string {
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	if len(b) == 0 {
		return "0"
	}
	return string(b)
}

func sameLabels(a, b []string) bool {
	norm := func(l []string) []string {
		o := make([]string, len(l))
		for i, x := range l {
			o[i] = strings.ToLower(x)
		}
		slices.Sort(o)
		return o
	}
	return slices.Equal(norm(a), norm(b))
}

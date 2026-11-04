// Package issuetools implements the agent's read-only GitHub issue tools (issue_search,
// issue_get, issue_comments; ADR 0004). They run on the controller: the runner holds no
// GitHub credential. Reads come from the synced issue table, with a live fallback.
package issuetools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/issuestore"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/runs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

const (
	staleAfter       = 15 * time.Minute
	defaultBudget    = 60 // live GitHub calls per project per hour
	maxCommentPages  = 5
	defaultListLimit = 20
	maxSearchLimit   = 50
)

// Service serves the issue read tools.
type Service struct {
	DB  *db.DB
	GH  gh.API // nil when no GitHub App is configured: only synced rows can be served
	Now func() time.Time
	// LiveBudget is the number of live GitHub calls a project may make per hour (0 = 60).
	LiveBudget int

	mu    sync.Mutex
	spent map[int64][]time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// spend takes one live-call token for the project, or says when to retry.
func (s *Service) spend(project int64) error {
	budget := s.LiveBudget
	if budget <= 0 {
		budget = defaultBudget
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spent == nil {
		s.spent = map[int64][]time.Time{}
	}
	now := s.now()
	kept := s.spent[project][:0]
	for _, t := range s.spent[project] {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	if len(kept) >= budget {
		s.spent[project] = kept
		retry := time.Hour - now.Sub(kept[0])
		return fmt.Errorf("%w: this project used its %d live GitHub calls for the hour; retry in %d seconds (synced issues are still readable)",
			errs.Invalid, budget, int(retry.Seconds())+1)
	}
	s.spent[project] = append(kept, now)
	return nil
}

type args struct {
	Project string   `json:"project"`
	Repo    string   `json:"repo"`
	Ref     string   `json:"ref"`
	Number  int64    `json:"number"`
	Query   string   `json:"query"`
	State   string   `json:"state"`
	Labels  []string `json:"labels"`
	Limit   int      `json:"limit"`
	Live    bool     `json:"live"`
	After   string   `json:"after"`
}

func parse(raw json.RawMessage) (args, error) {
	var a args
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return a, fmt.Errorf("%w: arguments: %v", errs.Invalid, err)
		}
	}
	return a, nil
}

// target is the project and the repos an agent may touch: only those linked to its project.
type target struct {
	project int64
	repos   []dbgen.Repo
}

func (s *Service) scope(ctx context.Context, sc runs.Scope, a args) (target, error) {
	pid := sc.ProjectID // an agent can never name another project
	if pid == 0 {
		if a.Project == "" {
			return target{}, fmt.Errorf("%w: project is required", errs.Invalid)
		}
		p, err := s.DB.RQ.GetProjectBySlug(ctx, a.Project)
		if errors.Is(err, sql.ErrNoRows) {
			return target{}, fmt.Errorf("%w: project %q", errs.NotFound, a.Project)
		} else if err != nil {
			return target{}, err
		}
		pid = p.ID
	}
	rows, err := s.DB.RQ.ListRepos(ctx, pid)
	if err != nil {
		return target{}, err
	}
	t := target{project: pid}
	for _, r := range rows {
		t.repos = append(t.repos, r.Repo)
	}
	return t, nil
}

// pick chooses the repo named by repo/ref; with none named, the project's only repo.
// Returns nil (no error) when no repo was named and several are linked and allowNone is set.
func (t target) pick(repo string, allowNone bool) (*dbgen.Repo, error) {
	if repo == "" {
		switch {
		case len(t.repos) == 1:
			return &t.repos[0], nil
		case allowNone:
			return nil, nil
		case len(t.repos) == 0:
			return nil, fmt.Errorf("%w: this project has no linked GitHub repository", errs.NotFound)
		}
		return nil, fmt.Errorf("%w: repo is required (owner/name); linked repos: %s", errs.Invalid, t.names())
	}
	for i, r := range t.repos {
		if strings.EqualFold(r.Owner+"/"+r.Name, repo) {
			return &t.repos[i], nil
		}
	}
	// Not linked to this project: invisible, like a task of another project.
	return nil, fmt.Errorf("%w: repo %q is not linked to this project", errs.NotFound, repo)
}

func (t target) names() string {
	var n []string
	for _, r := range t.repos {
		n = append(n, r.Owner+"/"+r.Name)
	}
	return strings.Join(n, ", ")
}

// repoAndNumber resolves repo and issue number from repo+number or ref (owner/name#n).
func (t target) repoAndNumber(a args) (*dbgen.Repo, int64, error) {
	repo, num := a.Repo, a.Number
	if a.Ref != "" {
		r, n, ok := strings.Cut(a.Ref, "#")
		if !ok {
			return nil, 0, fmt.Errorf("%w: ref %q: want owner/name#number", errs.Invalid, a.Ref)
		}
		var v int64
		if _, err := fmt.Sscanf(n, "%d", &v); err != nil || v <= 0 || fmt.Sprint(v) != strings.TrimSpace(n) {
			return nil, 0, fmt.Errorf("%w: ref %q: want owner/name#number", errs.Invalid, a.Ref)
		}
		if repo != "" && !strings.EqualFold(repo, r) {
			return nil, 0, fmt.Errorf("%w: repo and ref disagree", errs.Invalid)
		}
		repo, num = r, v
	}
	if num <= 0 {
		return nil, 0, fmt.Errorf("%w: number (or ref) is required", errs.Invalid)
	}
	r, err := t.pick(repo, false)
	return r, num, err
}

func ghRepo(r dbgen.Repo) gh.Repo {
	return gh.Repo{Owner: r.Owner, Name: r.Name, DefaultBranch: r.DefaultBranch, CloneURL: r.CloneUrl, InstallationID: r.GhInstallationID.Int64}
}

func ref(r issuestore.Row) string { return fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number) }

var errNoGitHub = fmt.Errorf("%w: GitHub is not configured on this controller", errs.Invalid)

func (s *Service) live(project int64) (gh.IssueReader, error) {
	if s.GH == nil {
		return nil, errNoGitHub
	}
	if err := s.spend(project); err != nil {
		return nil, err
	}
	return s.GH, nil
}

func mapGH(r dbgen.Repo, n int64, err error) error {
	switch {
	case errors.Is(err, gh.ErrIsPullRequest):
		return fmt.Errorf("%w: %s/%s#%d is a pull request; the issue tools only handle issues", errs.Invalid, r.Owner, r.Name, n)
	case errors.Is(err, gh.ErrIssueNotFound):
		return fmt.Errorf("%w: issue %s/%s#%d", errs.NotFound, r.Owner, r.Name, n)
	}
	return err
}

// issue returns an issue from the table, refreshing a stale row (or fetching a missing one)
// from GitHub. stale reports a row served although a refresh failed.
func (s *Service) issue(ctx context.Context, t target, r dbgen.Repo, n int64, force bool) (row issuestore.Row, stale bool, err error) {
	row, err = issuestore.Get(ctx, s.DB, r.ID, n)
	have := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return row, false, err
	}
	if have {
		at, ok := issuestore.ParseTime(row.SyncedAt)
		if !force && row.GhUpdatedAt != "" && ok && s.now().Sub(at) < staleAfter {
			return row, false, nil
		}
	}
	fetch := func() error {
		rd, err := s.live(t.project)
		if err != nil {
			return err
		}
		i, err := rd.GetIssue(ctx, ghRepo(r), n)
		if err != nil {
			return mapGH(r, n, err)
		}
		if _, _, err := issuestore.Save(ctx, s.DB, r.ID, i, s.now()); err != nil {
			return err
		}
		row, err = issuestore.Get(ctx, s.DB, r.ID, n)
		return err
	}
	if ferr := fetch(); ferr != nil {
		if have && !force && !errors.Is(ferr, errs.NotFound) {
			return row, true, nil // serve what we synced; say it may be out of date
		}
		return issuestore.Row{}, false, ferr
	}
	return row, false, nil
}

func (s *Service) labelsOut(r issuestore.Row) []string {
	out := []string{}
	for _, l := range r.Labels {
		out = append(out, wrapShort(ref(r), "label", l))
	}
	return out
}

func (s *Service) summary(r issuestore.Row, snippet string) map[string]any {
	m := map[string]any{
		"ref": ref(r), "repo": r.Owner + "/" + r.Repo, "number": r.Number, "state": r.State, "url": r.URL,
		"title": Wrap(ref(r), "title", r.Title), "labels": s.labelsOut(r),
		"author": wrapShort(ref(r), "author", r.Author), "updated_at": r.GhUpdatedAt,
	}
	if snippet != "" {
		m["snippet"] = Wrap(ref(r), "snippet", snippet)
	}
	if r.TaskSqid != "" {
		m["linked_task"] = r.TaskSqid
	}
	return m
}

// Get implements issue_get.
func (s *Service) Get(ctx context.Context, sc runs.Scope, raw json.RawMessage) (any, error) {
	a, err := parse(raw)
	if err != nil {
		return nil, err
	}
	t, err := s.scope(ctx, sc, a)
	if err != nil {
		return nil, err
	}
	r, n, err := t.repoAndNumber(a)
	if err != nil {
		return nil, err
	}
	row, stale, err := s.issue(ctx, t, *r, n, a.Live)
	if err != nil {
		return nil, err
	}
	m := s.summary(row, "")
	m["body"] = Wrap(ref(row), "body", row.Body)
	m["comment_count"] = row.CommentCount
	m["synced_at"] = row.SyncedAt
	m["stale"] = stale
	m["untrusted"], m["notice"] = true, Notice
	return m, nil
}

// Search implements issue_search.
func (s *Service) Search(ctx context.Context, sc runs.Scope, raw json.RawMessage) (any, error) {
	a, err := parse(raw)
	if err != nil {
		return nil, err
	}
	t, err := s.scope(ctx, sc, a)
	if err != nil {
		return nil, err
	}
	var repo *dbgen.Repo
	if a.Ref != "" && a.Repo == "" {
		a.Repo, _, _ = strings.Cut(a.Ref, "#")
	}
	if repo, err = t.pick(a.Repo, true); err != nil {
		return nil, err
	}
	match := issuestore.FTSQuery(a.Query)
	if match == "" {
		return nil, fmt.Errorf("%w: query is required", errs.Invalid)
	}
	state := a.State
	switch state {
	case "":
		state = "open"
	case "open", "closed", "all":
	default:
		return nil, fmt.Errorf("%w: state must be open, closed or all", errs.Invalid)
	}
	limit := a.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	limit = min(limit, maxSearchLimit)
	var repoID int64
	targets := t.repos
	if repo != nil {
		repoID, targets = repo.ID, []dbgen.Repo{*repo}
	}
	fetch := limit
	if len(a.Labels) > 0 {
		fetch = 200 // labels are filtered after the text match
	}
	out := func(hits []issuestore.SearchHit, source string) map[string]any {
		res := []map[string]any{}
		for _, h := range hits {
			if len(res) == limit {
				break
			}
			if !hasLabels(h.Labels, a.Labels) {
				continue
			}
			res = append(res, s.summary(h.Row, h.Snippet))
		}
		return map[string]any{"results": res, "source": source, "untrusted": true, "notice": Notice}
	}
	if !a.Live {
		hits, err := issuestore.Search(ctx, s.DB, t.project, repoID, match, state, fetch)
		if err != nil {
			return nil, err
		}
		if len(hits) > 0 || !s.anyUnsynced(ctx, targets) {
			return out(hits, "db"), nil
		}
	}
	// Live: asked for, or nothing found and some repo has never been synced.
	if s.GH == nil {
		if a.Live {
			return nil, errNoGitHub
		}
		return out(nil, "db"), nil
	}
	var rows []issuestore.Row
	for _, r := range targets {
		if !a.Live {
			if n, _ := issuestore.Count(ctx, s.DB, r.ID); n > 0 {
				continue
			}
		}
		rd, err := s.live(t.project)
		if err != nil {
			if len(rows) > 0 {
				break
			}
			return nil, err
		}
		found, err := rd.SearchIssues(ctx, ghRepo(r), strings.TrimSpace(a.Query), state, fetch)
		if err != nil {
			return nil, err
		}
		for _, i := range found {
			if _, _, err := issuestore.Save(ctx, s.DB, r.ID, i, s.now()); err != nil {
				return nil, err
			}
			if row, err := issuestore.Get(ctx, s.DB, r.ID, i.Number); err == nil {
				rows = append(rows, row)
			}
		}
	}
	hits := make([]issuestore.SearchHit, 0, len(rows))
	for _, r := range rows {
		hits = append(hits, issuestore.SearchHit{Row: r})
	}
	return out(hits, "live"), nil
}

func (s *Service) anyUnsynced(ctx context.Context, repos []dbgen.Repo) bool {
	for _, r := range repos {
		if n, err := issuestore.Count(ctx, s.DB, r.ID); err == nil && n == 0 {
			return true
		}
	}
	return false
}

func hasLabels(have, want []string) bool {
	for _, w := range want {
		if !slices.ContainsFunc(have, func(h string) bool { return strings.EqualFold(h, w) }) {
			return false
		}
	}
	return true
}

// Comments implements issue_comments.
func (s *Service) Comments(ctx context.Context, sc runs.Scope, raw json.RawMessage) (any, error) {
	a, err := parse(raw)
	if err != nil {
		return nil, err
	}
	t, err := s.scope(ctx, sc, a)
	if err != nil {
		return nil, err
	}
	r, n, err := t.repoAndNumber(a)
	if err != nil {
		return nil, err
	}
	var after int64
	if a.After != "" {
		if _, err := fmt.Sscanf(a.After, "%d", &after); err != nil || after < 0 {
			return nil, fmt.Errorf("%w: after must be a cursor from a previous result", errs.Invalid)
		}
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 30
	}
	limit = min(limit, 100)
	row, _, err := s.issue(ctx, t, *r, n, false)
	if err != nil {
		return nil, err
	}
	stale := false
	cached, err := issuestore.CommentCount(ctx, s.DB, row.ID)
	if err != nil {
		return nil, err
	}
	if cached != row.CommentCount {
		if ferr := s.refreshComments(ctx, t, *r, row); ferr != nil {
			if cached == 0 && row.CommentCount > 0 {
				return nil, ferr
			}
			stale = true
		}
	}
	cs, err := issuestore.CachedComments(ctx, s.DB, row.ID, after, limit)
	if err != nil {
		return nil, err
	}
	res := []map[string]any{}
	next := ""
	for i, c := range cs {
		if i == limit {
			next = fmt.Sprint(cs[i-1].GhID)
			break
		}
		res = append(res, map[string]any{"id": c.GhID, "author": wrapShort(ref(row), "author", c.Author), "created_at": c.Time,
			"body": Wrap(ref(row), fmt.Sprintf("comment %d", c.GhID), c.Body)})
	}
	m := map[string]any{"ref": ref(row), "comments": res, "stale": stale, "untrusted": true, "notice": Notice}
	if next != "" {
		m["next"] = next
	}
	return m, nil
}

func (s *Service) refreshComments(ctx context.Context, t target, r dbgen.Repo, row issuestore.Row) error {
	var all []gh.IssueComment
	complete := false
	for page := 1; page <= maxCommentPages; page++ {
		rd, err := s.live(t.project)
		if err != nil {
			return err
		}
		cs, more, err := rd.IssueComments(ctx, ghRepo(r), row.Number, page)
		if err != nil {
			return mapGH(r, row.Number, err)
		}
		all = append(all, cs...)
		if !more {
			complete = true
			break
		}
	}
	return issuestore.ReplaceComments(ctx, s.DB, row.ID, all, complete)
}

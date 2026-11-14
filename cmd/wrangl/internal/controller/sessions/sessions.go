// Package sessions is the controller's reconciler for primary Claude sessions (ADR 0011 section 3).
// For every active project with a linked repo it keeps one session running on the session host,
// restarts it with `claude -c` and a backoff when it exits, and stops it when the project is
// archived or loses its repo. It holds no PTYs: the session host does.
package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/dbtime"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/sessionhost"
)

// Host is the part of the session host the reconciler uses (its socket client, or a fake).
type Host interface {
	List(ctx context.Context) ([]sessionhost.Info, error)
	Start(ctx context.Context, req sessionhost.Request) (sessionhost.Info, error)
	Stop(ctx context.Context, slug string) error
}

// Backoff bounds between starts of a session that keeps ending, and how long a session must stay up
// before its failure count is forgotten.
const (
	MinBackoff  = 5 * time.Second
	MaxBackoff  = 10 * time.Minute
	StableAfter = 5 * time.Minute
)

// Reconciler keeps the primary sessions in line with the projects.
type Reconciler struct {
	DB   *db.DB
	Host Host
	Dir  string // checkouts live at <Dir>/<slug>
	// Token returns a git token for the repo's one clone or fetch ("" = none, fine for public repos).
	Token func(ctx context.Context, r dbgen.Repo) (string, error)
	Now   func() time.Time
	Log   *slog.Logger
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

type row struct {
	state              string
	pid                int64
	started, next      time.Time
	restarts, failures int64
	everStarted        bool
	lastError          string
	exitCode           sql.NullInt64
	exists             bool
}

func (r *Reconciler) load(ctx context.Context, projectID int64) (row, error) {
	var x row
	var started, next sql.NullString
	err := r.DB.R.QueryRowContext(ctx, `SELECT state, pid, started_at, exit_code, restarts, failures, ever_started, next_attempt_at, last_error
		FROM primary_session WHERE project_id = ?`, projectID).Scan(&x.state, &x.pid, &started, &x.exitCode, &x.restarts, &x.failures, &x.everStarted, &next, &x.lastError)
	if err == sql.ErrNoRows {
		return row{state: "pending"}, nil
	}
	if err != nil {
		return x, err
	}
	x.exists = true
	if started.Valid {
		x.started, _ = dbtime.Parse(started.String)
	}
	if next.Valid {
		x.next, _ = dbtime.Parse(next.String)
	}
	return x, nil
}

func nstr(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: dbtime.Format(t), Valid: true}
}

func (r *Reconciler) save(ctx context.Context, projectID int64, x row) error {
	_, err := r.DB.W.ExecContext(ctx, `INSERT INTO primary_session
		(project_id, state, pid, started_at, exit_code, restarts, failures, ever_started, next_attempt_at, last_error, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(project_id) DO UPDATE SET state=excluded.state, pid=excluded.pid, started_at=excluded.started_at,
		  exit_code=excluded.exit_code, restarts=excluded.restarts, failures=excluded.failures, ever_started=excluded.ever_started,
		  next_attempt_at=excluded.next_attempt_at, last_error=excluded.last_error, updated_at=excluded.updated_at`,
		projectID, x.state, x.pid, nstr(x.started), x.exitCode, x.restarts, x.failures, x.everStarted, nstr(x.next), x.lastError, dbtime.Format(r.now()))
	return err
}

func backoff(failures int64) time.Duration {
	d := MinBackoff
	for i := int64(1); i < failures && d < MaxBackoff; i++ {
		d *= 2
	}
	return min(d, MaxBackoff)
}

// Reconcile makes one pass. It is safe to call often; it does nothing for a session that is up.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	all, err := r.DB.R.QueryContext(ctx, `SELECT p.id, p.slug, p.archived_at IS NOT NULL, COALESCE(p.default_repo_id, 0) FROM project p ORDER BY p.slug`)
	if err != nil {
		return err
	}
	type proj struct {
		id       int64
		slug     string
		archived bool
		defRepo  int64
	}
	var projs []proj
	for all.Next() {
		var p proj
		if err := all.Scan(&p.id, &p.slug, &p.archived, &p.defRepo); err != nil {
			all.Close()
			return err
		}
		projs = append(projs, p)
	}
	all.Close()
	repos, err := r.DB.RQ.ListRepos(ctx, 0)
	if err != nil {
		return err
	}
	firstRepo := map[int64]dbgen.Repo{}
	byProject := map[int64][]dbgen.Repo{}
	for _, x := range repos {
		if _, ok := firstRepo[x.Repo.ProjectID]; !ok {
			firstRepo[x.Repo.ProjectID] = x.Repo
		}
		byProject[x.Repo.ProjectID] = append(byProject[x.Repo.ProjectID], x.Repo)
	}

	live, err := r.Host.List(ctx)
	if err != nil {
		for _, p := range projs {
			if _, ok := firstRepo[p.id]; !ok || p.archived {
				continue
			}
			x, lerr := r.load(ctx, p.id)
			if lerr != nil {
				return lerr
			}
			x.state, x.lastError = "host_down", "session host unreachable: "+err.Error()
			if err := r.save(ctx, p.id, x); err != nil {
				return err
			}
		}
		return fmt.Errorf("session host: %w", err)
	}
	bySlug := map[string]sessionhost.Info{}
	for _, i := range live {
		bySlug[i.Slug] = i
	}

	for _, p := range projs {
		repo, hasRepo := firstRepo[p.id]
		info, have := bySlug[p.slug]
		x, err := r.load(ctx, p.id)
		if err != nil {
			return err
		}
		if p.archived || !hasRepo {
			if have { // stop on archive (or when the last repo is unlinked)
				if err := r.Host.Stop(ctx, p.slug); err != nil {
					r.log().Warn("stop primary session", "project", p.slug, "err", err)
					continue
				}
			}
			if x.exists && x.state != "stopped" {
				x.state, x.pid, x.lastError, x.next = "stopped", 0, "", time.Time{}
				if err := r.save(ctx, p.id, x); err != nil {
					return err
				}
			}
			continue
		}
		// The session starts in the project's default repo (else the first linked); with several repos
		// they all sit side by side under <dir>/<slug>/ (ADR 0012 section 8).
		all := byProject[p.id]
		for _, c := range all {
			if c.ID == p.defRepo {
				repo = c
			}
		}
		var others []dbgen.Repo
		if len(all) > 1 {
			for _, c := range all {
				if c.ID != repo.ID {
					others = append(others, c)
				}
			}
		}
		if err := r.ensure(ctx, p.id, p.slug, repo, others, info, have, x); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) ensure(ctx context.Context, id int64, slug string, repo dbgen.Repo, others []dbgen.Repo, info sessionhost.Info, have bool, x row) error {
	now := r.now()
	if have && info.State == "running" {
		x.state, x.pid, x.started, x.lastError, x.next = "running", int64(info.PID), info.Started, "", time.Time{}
		x.exitCode = sql.NullInt64{}
		x.everStarted = true
		if now.Sub(info.Started) >= StableAfter {
			x.failures = 0
		}
		return r.save(ctx, id, x)
	}
	if have && info.State == "exited" && (x.state == "running" || x.state == "pending" || !x.exists) {
		// First sight of this exit: count it, schedule the restart.
		x.failures++
		x.exitCode = sql.NullInt64{Int64: int64(info.ExitCode), Valid: true}
		x.pid = 0
		if now.Sub(info.Started) >= StableAfter {
			x.failures = 1
		}
		x.next = now.Add(backoff(x.failures))
		x.state, x.lastError = "backoff", fmt.Sprintf("exited with code %d", info.ExitCode)
		if err := r.save(ctx, id, x); err != nil {
			return err
		}
	}
	if (x.state == "backoff" || x.state == "failed") && now.Before(x.next) {
		return nil
	}
	// Start (or start again). The host continues the old conversation with -c when this session has
	// run before, even if the host itself lost the record (it restarted).
	dir := filepath.Join(r.Dir, slug)
	if len(others) > 0 { // several repos: <dir>/<slug>/<repo name>, the session in the primary one
		dir = filepath.Join(dir, repo.Name)
	}
	req := sessionhost.StartRequest(slug, dir, slug)
	req.CloneURL, req.Branch = repoURL(repo), repo.DefaultBranch
	for _, o := range others {
		c := sessionhost.Checkout{Dir: filepath.Join(r.Dir, slug, o.Name), CloneURL: repoURL(o), Branch: o.DefaultBranch}
		if r.Token != nil {
			if tok, err := r.Token(ctx, o); err == nil {
				c.Token = tok
			}
		}
		req.Extra = append(req.Extra, c)
	}
	req.Resume = x.everStarted
	if r.Token != nil {
		if tok, err := r.Token(ctx, repo); err != nil {
			r.log().Warn("primary session: git token", "project", slug, "err", err)
		} else {
			req.Token = tok
		}
	}
	started, err := r.Host.Start(ctx, req)
	if err != nil {
		x.failures++
		x.state, x.lastError, x.pid = "failed", err.Error(), 0
		x.next = now.Add(backoff(x.failures))
		r.log().Warn("primary session did not start", "project", slug, "err", err, "retry_in", backoff(x.failures))
		return r.save(ctx, id, x)
	}
	if x.everStarted {
		x.restarts++
	}
	x.everStarted = true
	x.state, x.pid, x.started, x.lastError, x.next = "running", int64(started.PID), started.Started, "", time.Time{}
	x.exitCode = sql.NullInt64{}
	r.log().Info("primary session started", "project", slug, "pid", started.PID, "resumed", started.Resumed)
	return r.save(ctx, id, x)
}

func repoURL(r dbgen.Repo) string {
	if r.CloneUrl != "" {
		return r.CloneUrl
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", r.Owner, r.Name)
}

// Loop reconciles every interval until ctx ends.
func (r *Reconciler) Loop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := r.Reconcile(ctx); err != nil && ctx.Err() == nil {
			r.log().Warn("primary sessions", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Health is the one-line status shown on the project page ("" when the project has no session
// record, which is every project unless the reconciler is configured).
func (r *Reconciler) Health(ctx context.Context, slug string) string {
	var id int64
	if err := r.DB.R.QueryRowContext(ctx, `SELECT id FROM project WHERE slug = ?`, slug).Scan(&id); err != nil {
		return ""
	}
	x, err := r.load(ctx, id)
	if err != nil || !x.exists {
		return ""
	}
	now := r.now()
	switch x.state {
	case "running":
		s := fmt.Sprintf("running, pid %d, up %s", x.pid, round(now.Sub(x.started)))
		if x.restarts > 0 {
			s += fmt.Sprintf(", restarted %d time%s", x.restarts, plural(x.restarts))
		}
		return s
	case "backoff":
		return fmt.Sprintf("ended (%s); restarting in %s", x.lastError, round(max(x.next.Sub(now), 0)))
	case "failed":
		return fmt.Sprintf("could not start: %s; retrying in %s", firstLine(x.lastError), round(max(x.next.Sub(now), 0)))
	case "host_down":
		return firstLine(x.lastError)
	case "stopped":
		return "stopped"
	}
	return "starting"
}

func plural(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func round(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours())/24)
}

// SocketHost adapts a session host socket client to Host.
type SocketHost struct{ C sessionhost.Client }

func (s SocketHost) List(ctx context.Context) ([]sessionhost.Info, error) {
	r, err := s.C.Do(sessionhost.Request{Op: sessionhost.OpList})
	return r.Sessions, err
}

func (s SocketHost) Start(ctx context.Context, req sessionhost.Request) (sessionhost.Info, error) {
	r, err := s.C.Do(req)
	if err != nil {
		return sessionhost.Info{}, err
	}
	return *r.Session, nil
}

func (s SocketHost) Stop(ctx context.Context, slug string) error {
	_, err := s.C.Do(sessionhost.Request{Op: sessionhost.OpStop, Slug: slug})
	return err
}

// State is the pill value for a project: "running", "restarting" (starting, backing off, failed or
// the host is down: it is expected to come back) or "none" (no session, stopped, or unconfigured).
func (r *Reconciler) State(ctx context.Context, slug string) string {
	var id int64
	if err := r.DB.R.QueryRowContext(ctx, `SELECT id FROM project WHERE slug = ?`, slug).Scan(&id); err != nil {
		return "none"
	}
	x, err := r.load(ctx, id)
	if err != nil || !x.exists {
		return "none"
	}
	switch x.state {
	case "running":
		return "running"
	case "stopped":
		return "none"
	}
	return "restarting"
}

// Client is the session host socket client, for the web terminal's attach.
func (r *Reconciler) Client() sessionhost.Client {
	if sh, ok := r.Host.(SocketHost); ok {
		return sh.C
	}
	return sessionhost.Client{}
}

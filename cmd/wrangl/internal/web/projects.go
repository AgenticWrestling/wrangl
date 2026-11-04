package web

import (
	"context"
	"sort"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
)

// projectRow is one line of the Projects list: the project plus its live counts.
type projectRow struct {
	dbgen.Project
	Runners    int // distinct runners hosting one of its active runs
	InProgress int // active runs (assigned .. pushing)
	Queued     int // queued runs
	Backlog    int // open top-level tasks with no active or queued run
	Inbox      int // open questions plus open pull requests
	RunnerList []projectRunner
	Recent     []projectActivity
}

type projectRunner struct {
	Name     string
	Sessions int
	Also     []string // other projects this runner is serving too ("also serving X")
}

type projectActivity struct {
	Sqid, Title, State, At string
}

// activeRunStates are the run states that occupy a runner session.
const activeRunStates = `'assigned','preparing','running','needs_input','blocked','pushing'`

// projectRows aggregates the counts for the given projects with a handful of queries (not one per project).
func projectRows(ctx context.Context, d *db.DB, projects []dbgen.Project) ([]projectRow, error) {
	rows := make([]projectRow, len(projects))
	idx := map[int64]int{}
	for i, p := range projects {
		rows[i] = projectRow{Project: p}
		idx[p.ID] = i
	}
	// Counts per project.
	counts, err := d.R.QueryContext(ctx, `
SELECT t.project_id,
  SUM(r.state IN (`+activeRunStates+`)),
  SUM(r.state = 'queued')
FROM run r JOIN task t ON t.id = r.task_id
WHERE r.state IN (`+activeRunStates+`,'queued') GROUP BY t.project_id`)
	if err != nil {
		return nil, err
	}
	for counts.Next() {
		var id int64
		var prog, queued int
		if err := counts.Scan(&id, &prog, &queued); err != nil {
			counts.Close()
			return nil, err
		}
		if i, ok := idx[id]; ok {
			rows[i].InProgress, rows[i].Queued = prog, queued
		}
	}
	counts.Close()

	for _, q := range []struct {
		sql string
		set func(*projectRow, int)
	}{
		{`SELECT t.project_id, COUNT(*) FROM task t WHERE t.parent_id IS NULL AND t.state = 'open'
		  AND NOT EXISTS (SELECT 1 FROM run r WHERE r.task_id = t.id AND r.state IN (` + activeRunStates + `,'queued'))
		  GROUP BY t.project_id`, func(r *projectRow, n int) { r.Backlog = n }},
		{`SELECT t.project_id, COUNT(*) FROM question q JOIN task t ON t.id = q.task_id WHERE q.state = 'open' GROUP BY t.project_id`,
			func(r *projectRow, n int) { r.Inbox += n }},
		{`SELECT t.project_id, COUNT(*) FROM pull_request pr JOIN task t ON t.id = pr.task_id WHERE pr.state = 'open' GROUP BY t.project_id`,
			func(r *projectRow, n int) { r.Inbox += n }},
	} {
		rs, err := d.R.QueryContext(ctx, q.sql)
		if err != nil {
			return nil, err
		}
		for rs.Next() {
			var id int64
			var n int
			if err := rs.Scan(&id, &n); err != nil {
				rs.Close()
				return nil, err
			}
			if i, ok := idx[id]; ok {
				q.set(&rows[i], n)
			}
		}
		rs.Close()
	}

	// Runners now, with the projects each one is serving (a runner may host sessions of several).
	rs, err := d.R.QueryContext(ctx, `
SELECT t.project_id, ru.name, COUNT(*) FROM run r JOIN task t ON t.id = r.task_id JOIN runner ru ON ru.id = r.runner_id
WHERE r.state IN (`+activeRunStates+`) GROUP BY t.project_id, ru.id ORDER BY ru.name`)
	if err != nil {
		return nil, err
	}
	type use struct {
		project int64
		runner  string
		n       int
	}
	var uses []use
	serving := map[string][]int64{}
	for rs.Next() {
		var u use
		if err := rs.Scan(&u.project, &u.runner, &u.n); err != nil {
			rs.Close()
			return nil, err
		}
		uses = append(uses, u)
		serving[u.runner] = append(serving[u.runner], u.project)
	}
	rs.Close()
	slugs := map[int64]string{}
	if all, err := d.R.QueryContext(ctx, `SELECT id, slug FROM project`); err == nil {
		for all.Next() {
			var id int64
			var s string
			all.Scan(&id, &s)
			slugs[id] = s
		}
		all.Close()
	}
	for _, u := range uses {
		i, ok := idx[u.project]
		if !ok {
			continue
		}
		pr := projectRunner{Name: u.runner, Sessions: u.n}
		for _, other := range serving[u.runner] {
			if other != u.project {
				pr.Also = append(pr.Also, slugs[other])
			}
		}
		sort.Strings(pr.Also)
		rows[i].RunnerList = append(rows[i].RunnerList, pr)
		rows[i].Runners++
	}

	// Recent activity: the latest few touched tasks.
	rs, err = d.R.QueryContext(ctx, `SELECT project_id, COALESCE(sqid,''), title, state, updated_at FROM task ORDER BY updated_at DESC LIMIT 400`)
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		var id int64
		var a projectActivity
		if err := rs.Scan(&id, &a.Sqid, &a.Title, &a.State, &a.At); err != nil {
			rs.Close()
			return nil, err
		}
		if i, ok := idx[id]; ok && len(rows[i].Recent) < 3 {
			rows[i].Recent = append(rows[i].Recent, a)
		}
	}
	rs.Close()
	return rows, nil
}

// firstLine is the project description's first non-empty line, markdown headings stripped, for the "about" column.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#>*- ")); l != "" {
			return l
		}
	}
	return ""
}

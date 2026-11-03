// Package web is the mobile-first PWA (spec §11): server-rendered html/template pages,
// made live with Datastar (vendored, served from the binary): every page opens one
// @get stream that morph-replaces its #main region whenever the event bus fires.
// Actions post Datastar signals to /ui/act/* and answer with a toast. Drag and drop
// (SortableJS) and the terminal (xterm.js over the PTY SSE stream) live in app.js.
package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nreid/devhost/cmd/wrangl/internal/api"
	"github.com/nreid/devhost/cmd/wrangl/internal/controller/projects"
	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbgen"
	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbtypes"
	"github.com/nreid/devhost/cmd/wrangl/internal/errs"
	"github.com/nreid/devhost/cmd/wrangl/internal/proto"
	"github.com/nreid/devhost/cmd/wrangl/internal/stack"
)

//go:embed static templates
var assets embed.FS

// Handler serves the PWA.
type Handler struct {
	D    api.Deps
	tmpl *template.Template
	css  string // chroma styles (light + dark)

	sessions sync.Map // page session id -> *liveSession
}

// liveSession is one open page's live stream. The page says which backlog task it has
// expanded, so every re-render of the list includes that task's card (a client-side patch
// would be wiped by the next morph).
type liveSession struct {
	mu       sync.Mutex
	expanded string // sqid
	kick     chan struct{}
}

func (s *liveSession) get() string { s.mu.Lock(); defer s.mu.Unlock(); return s.expanded }

func newSID() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// New parses the templates.
func New(d api.Deps) (*Handler, error) {
	h := &Handler{D: d, css: chromaCSS()}
	t, err := template.New("").Funcs(template.FuncMap{
		"glyph": func(s string) string { return glyph[s] },
		"ago":   ago,
		"md":    markdown,
		"pct":   func(v float64) string { return fmt.Sprintf("%.0f%%", v) },
		"spark": sparkline,
		"json":  func(v any) template.JS { b, _ := json.Marshal(v); return template.JS(b) },
		"q":     url.QueryEscape,
		"add":   func(a, b int) int { return a + b },
		"pctof": func(a, b int64) int64 {
			if b == 0 {
				return 0
			}
			return a * 100 / b
		},
		"dur":       func(s int64) string { return (time.Duration(s) * time.Second).String() },
		"list":      func(v ...string) []string { return v },
		"hasSuffix": strings.HasSuffix,
		"lower":     strings.ToLower,
		"outline":   outlineFromPayload,
		"ticon":     typeIcon,
		"termok":    termUsable,
		"faicon": func(n string) template.HTML {
			return template.HTML(`<svg class="fa" aria-hidden="true"><use href="#fa-` + n + `"/></svg>`)
		},
		"typeEdit": func(types []dbtypes.TaskType, cur dbtypes.TaskType) map[string]any {
			return map[string]any{"Types": types, "Cur": string(cur)}
		},
		"tlabel": func(t string) string { return dbtypes.TaskType(t).Label() },
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	h.tmpl = t
	return h, nil
}

var glyph = map[string]string{"open": "○", "in_progress": "▶", "in_review": "◐", "done": "✓", "cancelled": "✗"}

// Register mounts the PWA on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	static, _ := fs.Sub(assets, "static")
	fileSrv := http.FileServer(http.FS(static))
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "vendor/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // versioned file names
		}
		fileSrv.ServeHTTP(w, r)
	})))
	for _, f := range []string{"sw.js", "manifest.webmanifest", "icon.svg"} {
		f := f
		mux.HandleFunc("GET /"+f, func(w http.ResponseWriter, r *http.Request) {
			if f == "sw.js" {
				w.Header().Set("Service-Worker-Allowed", "/")
			}
			http.ServeFileFS(w, r, static, f)
		})
	}
	mux.HandleFunc("GET /icons/{size}", h.icon)
	mux.HandleFunc("GET /static/chroma.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		io.WriteString(w, h.css)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/backlog", http.StatusFound) })
	for path, view := range map[string]string{
		"/backlog": "backlog", "/task/{sqid}": "task", "/runners": "runners", "/runner/{name}": "runner",
		"/runs": "runs", "/run/{id}": "run", "/run/{id}/diff": "diff", "/run/{id}/files": "files",
		"/inbox": "inbox", "/settings": "settings", "/project/{slug}": "project", "/setup": "setup",
	} {
		view := view
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) { h.page(w, r, view) })
	}
	mux.HandleFunc("GET /runner/{name}/term", h.termPage)
	mux.HandleFunc("GET /setup/github/callback", h.githubCallback)
	mux.HandleFunc("GET /ui/live", h.live)
	mux.HandleFunc("GET /more", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/settings", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /ui/detail", h.detail)
	mux.HandleFunc("POST /ui/expand", h.expand)
	mux.HandleFunc("GET /deliverable/{id}", h.deliverable)
	mux.HandleFunc("POST /ui/act/{action}", h.act)
}

// page renders a full page: layout + the view's main region.
func (h *Handler) page(w http.ResponseWriter, r *http.Request, view string) {
	data, err := h.data(r.Context(), view, r)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, errs.NotFound) {
			code = http.StatusNotFound
		}
		http.Error(w, err.Error(), code)
		return
	}
	main, err := h.render("main-"+view, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := url.Values{"view": {view}}
	for k, v := range r.URL.Query() {
		q[k] = v
	}
	sid := newSID()
	q.Set("sid", sid)
	for _, k := range []string{"sqid", "name", "id", "slug"} {
		if v := r.PathValue(k); v != "" {
			q.Set(k, v)
		}
	}
	open, _ := h.D.Runs.Questions(r.Context(), false)
	page := map[string]any{"View": view, "Title": data["Title"], "Main": main, "LiveURL": "/ui/live?" + q.Encode(), "SID": sid,
		"Inbox": len(open), "Data": data}
	for _, region := range []string{"Pre", "Post"} { // static regions outside #main (sheets, terminal)
		name := strings.ToLower(region) + "-" + view
		if h.tmpl.Lookup(name) == nil {
			continue
		}
		html, err := h.render(name, data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		page[region] = html
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "layout", page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (h *Handler) render(name string, data any) (template.HTML, error) {
	var b bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&b, name, data); err != nil {
		return "", err
	}
	return template.HTML(b.String()), nil
}

// deliverable serves a stored image or report. The content is agent output, so it is served
// inert: fixed type, no sniffing, and a CSP that forbids everything.
func (h *Handler) deliverable(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || h.D.Deliverables == nil {
		http.NotFound(w, r)
		return
	}
	d, path, err := h.D.Deliverables.Open(r.Context(), id)
	if err != nil || path == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", d.Mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	io.Copy(w, f)
}

// deliverableView is a deliverable ready for the task page.
type deliverableView struct {
	dbgen.Deliverable
	Report template.HTML // rendered markdown, for reports
}

// taskPart renders one part of the task page ("main-task": the card; "post-task": its edit
// sheets) for a task, as an HTML fragment.
func (h *Handler) taskPart(ctx context.Context, sqid, tmpl string) (template.HTML, error) {
	req := (&http.Request{URL: &url.URL{RawQuery: url.Values{"sqid": {sqid}}.Encode()}}).WithContext(ctx)
	data, err := h.data(ctx, "task", req)
	if err != nil {
		return "", err
	}
	return h.render(tmpl, data)
}

// attachDetail puts card on the node with the given sqid.
func attachDetail(nodes []*node, sqid string, card template.HTML) bool {
	for _, n := range nodes {
		if n.Sqid == sqid {
			n.Detail = card
			return true
		}
		if attachDetail(n.Kids, sqid, card) {
			return true
		}
	}
	return false
}

// detail serves a part of a task page as a fragment: part=card (default) or part=sheets. The
// card is also included in the live re-render while a page has the task expanded (see
// expand); the sheets (dialogs) live outside #main so a re-render cannot close one.
func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	tmpl := "main-task"
	if r.URL.Query().Get("part") == "sheets" {
		tmpl = "post-task"
	}
	out, err := h.taskPart(r.Context(), r.URL.Query().Get("sqid"), tmpl)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, errs.NotFound) {
			code = http.StatusNotFound
		}
		http.Error(w, err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, string(out))
}

// expand records which task a page has expanded (sqid "" = none) and refreshes its stream.
func (h *Handler) expand(w http.ResponseWriter, r *http.Request) {
	v, ok := h.sessions.Load(r.URL.Query().Get("sid"))
	if !ok {
		w.WriteHeader(http.StatusNoContent) // not live (or reconnecting): the client fills the card itself
		return
	}
	s := v.(*liveSession)
	s.mu.Lock()
	s.expanded = r.URL.Query().Get("sqid")
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
	w.WriteHeader(http.StatusNoContent)
}

// fakeReq lets data() read path values from the live stream's query.
func reqValue(r *http.Request, k string) string {
	if v := r.PathValue(k); v != "" {
		return v
	}
	return r.URL.Query().Get(k)
}

// live is the page's Datastar stream: it re-renders #main on every change notice.
func (h *Handler) live(w http.ResponseWriter, r *http.Request) {
	view := r.URL.Query().Get("view")
	if _, ok := map[string]bool{"backlog": true, "task": true, "runners": true, "runner": true, "runs": true, "run": true, "inbox": true, "settings": true, "project": true}[view]; !ok {
		w.WriteHeader(http.StatusNoContent) // static pages (diff, files) are not live
		return
	}
	ch, stop := h.D.Bus.Subscribe()
	defer stop()
	sess := &liveSession{kick: make(chan struct{}, 1)}
	if sid := r.URL.Query().Get("sid"); sid != "" {
		h.sessions.Store(sid, sess)
		defer h.sessions.Delete(sid)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	push := func() {
		data, err := h.data(r.Context(), view, r)
		if err != nil {
			return
		}
		if sq := sess.get(); view == "backlog" && sq != "" {
			if card, err := h.taskPart(r.Context(), sq, "main-task"); err == nil {
				if roots, ok := data["Roots"].([]*node); ok {
					attachDetail(roots, sq, card)
				}
			}
		}
		main, err := h.render("main-"+view, data)
		if err != nil {
			return
		}
		open, _ := h.D.Runs.Questions(r.Context(), false)
		badge := fmt.Sprintf(`<span id="inbox-badge" class="badge">%s</span>`, map[bool]string{true: strconv.Itoa(len(open)), false: ""}[len(open) > 0])
		mergeFragments(w, `<main id="main">`+string(main)+`</main>`)
		mergeFragments(w, badge)
		if fl != nil {
			fl.Flush()
		}
	}
	push()
	keep := time.NewTicker(api.KeepAlive)
	defer keep.Stop()
	var debounce <-chan time.Time
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sess.kick:
			push()
		case <-ch:
			if debounce == nil {
				debounce = time.After(250 * time.Millisecond)
			}
		case <-debounce:
			debounce = nil
			push()
		case <-keep.C:
			io.WriteString(w, ": ping\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

// mergeFragments writes a Datastar merge-fragments event (one data line per HTML line).
func mergeFragments(w io.Writer, html string) {
	io.WriteString(w, "event: datastar-merge-fragments\n")
	for _, line := range strings.Split(html, "\n") {
		fmt.Fprintf(w, "data: fragments %s\n", line)
	}
	io.WriteString(w, "\n")
}

// mergeSignals writes a Datastar merge-signals event.
func mergeSignals(w io.Writer, signals map[string]any) {
	b, _ := json.Marshal(signals)
	fmt.Fprintf(w, "event: datastar-merge-signals\ndata: signals %s\n\n", b)
}

func executeScript(w io.Writer, js string) {
	io.WriteString(w, "event: datastar-execute-script\n")
	fmt.Fprintf(w, "data: script %s\n\n", strings.ReplaceAll(js, "\n", " "))
}

func toast(w http.ResponseWriter, msg string, isErr bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	cls := "toast show"
	if isErr {
		cls += " err"
	}
	mergeFragments(w, fmt.Sprintf(`<div id="toast" class="%s">%s</div>`, cls, template.HTMLEscapeString(msg)))
}

// --- view data -------------------------------------------------------------------------

type node struct {
	proto.TaskNode
	Attention bool          // an open question, or its latest run failed / is blocked
	Detail    template.HTML // the expanded task's card, when this page has it open
	Kids      []*node
}

func (h *Handler) data(ctx context.Context, view string, r *http.Request) (map[string]any, error) {
	db := h.D.Fleet.DB
	d := map[string]any{}
	switch view {
	case "backlog":
		projects, err := db.RQ.ListProjects(ctx)
		if err != nil {
			return nil, err
		}
		d["Projects"] = projects
		d["Recent"], d["Rest"] = recentProjects(projects, 4)
		d["Templates"], _ = db.RQ.ListTemplates(ctx)
		slug := r.URL.Query().Get("project")
		if slug == "" && len(projects) > 0 {
			slug = projects[0].Slug
		}
		d["Project"], d["Title"] = slug, "Backlog"
		d["Types"] = dbtypes.TaskTypes
		d["All"] = r.URL.Query().Get("all") == "1"
		if slug != "" {
			roots, err := h.tree(ctx, slug, d["All"].(bool))
			if err != nil {
				return nil, err
			}
			d["Roots"] = roots
		}
	case "task":
		sqid := reqValue(r, "sqid")
		t, err := h.D.Stack.Task(ctx, db.RQ, sqid)
		if err != nil {
			return nil, err
		}
		p, _ := db.RQ.GetProject(ctx, t.ProjectID)
		d["Task"], d["Project"], d["Title"] = t, p.Slug, t.Title
		d["Attention"] = h.attention(ctx, p.Slug)[sqid] && t.State != "done" && t.State != "cancelled"
		d["Types"] = dbtypes.TaskTypes
		if h.D.Deliverables != nil {
			rows, _ := db.RQ.ListDeliverablesForTask(ctx, t.ID)
			var dv []deliverableView
			for _, x := range rows {
				v := deliverableView{Deliverable: x}
				if x.Kind == "report_md" {
					if _, path, err := h.D.Deliverables.Open(ctx, x.ID); err == nil && path != "" {
						if b, err := os.ReadFile(path); err == nil {
							v.Report = markdown(string(b))
						}
					}
				}
				dv = append(dv, v)
			}
			d["Deliverables"] = dv
		}
		if t.ParentID.Valid {
			if pt, err := db.RQ.GetTask(ctx, t.ParentID.Int64); err == nil {
				d["Parent"] = pt
			}
		}
		kids, _ := db.RQ.ListChildren(ctx, sql.NullInt64{Int64: t.ID, Valid: true})
		d["Kids"] = kids
		d["Pre"], _ = db.RQ.ListPrecursors(ctx, t.ID)
		d["Dep"], _ = db.RQ.ListDependents(ctx, t.ID)
		d["Runs"], _ = db.RQ.RunsForTask(ctx, t.ID)
		d["Events"], _ = db.RQ.TaskEvents(ctx, sqid)
		if qid := r.URL.Query().Get("split"); qid != "" { // "Edit" on a plan proposal
			if id, err := strconv.ParseInt(qid, 10, 64); err == nil {
				if q, err := h.D.Runs.Question(ctx, id); err == nil {
					d["SplitOutline"] = outlineFromPayload(q.Payload)
				}
			}
		}
	case "runners":
		rows, err := db.RQ.ListRunners(ctx, 0)
		if err != nil {
			return nil, err
		}
		type card struct {
			R       dbgen.Runner
			T       dbgen.RunnerTemplate
			M       proto.Heartbeat
			Spark   []float64
			Run     *dbgen.Run
			IdleFor string
		}
		var cards []card
		for _, x := range rows {
			c := card{R: x.Runner}
			c.T, _ = db.RQ.GetTemplate(ctx, x.Runner.TemplateID)
			json.Unmarshal([]byte(x.Runner.LastMetricsJson), &c.M)
			for _, s := range h.D.Fleet.History(x.Runner.ID) {
				c.Spark = append(c.Spark, s.CPU)
			}
			if run, err := db.RQ.ActiveRunForRunner(ctx, sql.NullInt64{Int64: x.Runner.ID, Valid: true}); err == nil {
				c.Run = &run
			}
			cards = append(cards, c)
		}
		// Machines the fleet doesn't launch (the controller host) are permanent: pin them first.
		sort.SliceStable(cards, func(i, j int) bool {
			return cards[i].R.Launched == 0 && cards[j].R.Launched != 0
		})
		d["Cards"] = cards
		demand, _ := db.RQ.TemplateDemand(ctx)
		d["Demand"], d["Title"] = demand, "Runners"
	case "runner":
		r2, err := db.RQ.GetRunnerByName(ctx, reqValue(r, "name"))
		if err != nil {
			return nil, fmt.Errorf("%w: runner", errs.NotFound)
		}
		d["R"], d["Title"] = r2, r2.Name
		t, _ := db.RQ.GetTemplate(ctx, r2.TemplateID)
		d["T"] = t
		var m proto.Heartbeat
		json.Unmarshal([]byte(r2.LastMetricsJson), &m)
		d["M"] = m
		var spark []float64
		for _, s := range h.D.Fleet.History(r2.ID) {
			spark = append(spark, s.CPU)
		}
		d["Spark"] = spark
		if run, err := db.RQ.ActiveRunForRunner(ctx, sql.NullInt64{Int64: r2.ID, Valid: true}); err == nil {
			d["Run"] = run
			d["Progress"], _ = db.RQ.SubtreeOrdered(ctx, run.TaskID)
		}
		d["Recent"], _ = db.RQ.ListRuns(ctx, dbgen.ListRunsParams{Runner: r2.Name, Lim: 10})
	case "runs":
		rows, err := db.RQ.ListRuns(ctx, dbgen.ListRunsParams{Lim: 100})
		if err != nil {
			return nil, err
		}
		groups := map[string][]dbgen.ListRunsRow{}
		for _, x := range rows {
			g := x.Run.State
			switch g {
			case "assigned", "preparing", "running", "pushing", "cancelling":
				g = "running"
			case "succeeded", "failed", "cancelled":
				g = "recent"
			}
			groups[g] = append(groups[g], x)
		}
		d["Groups"], d["Order"], d["Title"] = groups, []string{"needs_input", "running", "blocked", "queued", "recent"}, "Runs"
	case "run", "diff", "files":
		id, _ := strconv.ParseInt(reqValue(r, "id"), 10, 64)
		run, err := db.RQ.GetRun(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("%w: run %d", errs.NotFound, id)
		}
		d["Run"], d["Title"] = run, fmt.Sprintf("Run %d", id)
		if t, err := db.RQ.GetTask(ctx, run.TaskID); err == nil {
			d["Task"] = t
		}
		if run.RunnerID.Valid {
			if rn, err := db.RQ.GetRunner(ctx, run.RunnerID.Int64); err == nil {
				d["Runner"] = rn.Name
			}
		}
		if pr, err := db.RQ.OpenPRForTask(ctx, run.TaskID); err == nil {
			d["PR"] = pr
		}
		switch view {
		case "run":
			q := r.URL.Query()
			logs, _ := db.RQ.RunLogs(ctx, dbgen.RunLogsParams{RunID: id, Stream: q.Get("stream"), Lim: 2000})
			if g := strings.ToLower(q.Get("q")); g != "" {
				var f []dbgen.LogLine
				for _, l := range logs {
					if strings.Contains(strings.ToLower(l.Message), g) {
						f = append(f, l)
					}
				}
				logs = f
			}
			if len(logs) > 300 {
				logs = logs[len(logs)-300:]
			}
			d["Logs"], d["Q"], d["Stream"] = logs, q.Get("q"), q.Get("stream")
			d["Hooks"], _ = db.RQ.RunHookEvents(ctx, id)
		case "diff":
			out, err := h.D.Runs.Inspect(ctx, id, "diff", "", false)
			d["Err"] = errString(err)
			d["Files"] = parseDiff(out.Data)
			d["Truncated"] = out.Truncated
		case "files":
			path := r.URL.Query().Get("path")
			d["Path"] = path
			kind := "tree"
			if path != "" && !strings.HasSuffix(path, "/") {
				kind = "file"
			}
			out, err := h.D.Runs.Inspect(ctx, id, kind, path, false)
			if err != nil && kind == "file" { // maybe a directory without the slash
				if o2, e2 := h.D.Runs.Inspect(ctx, id, "tree", path, false); e2 == nil {
					out, err, kind, path = o2, nil, "tree", path+"/"
					d["Path"] = path
				}
			}
			d["Err"], d["Kind"] = errString(err), kind
			if kind == "tree" {
				var entries []string
				for _, e := range strings.Split(strings.TrimSpace(out.Data), "\n") {
					if e != "" {
						entries = append(entries, e)
					}
				}
				d["Entries"] = entries
			} else {
				d["IsMD"] = strings.HasSuffix(strings.ToLower(path), ".md") && r.URL.Query().Get("raw") != "1"
				d["Content"] = out.Data
				d["Highlighted"] = highlight(path, out.Data)
			}
			d["Up"] = parentDir(path)
		}
	case "inbox":
		qs, err := h.D.Runs.Questions(ctx, false)
		if err != nil {
			return nil, err
		}
		prs, _ := db.RQ.ListPRs(ctx, "open")
		d["Questions"], d["PRs"], d["Title"] = qs, prs, "Inbox"
	case "project":
		p, err := h.D.Projects.Project(ctx, reqValue(r, "slug"))
		if err != nil {
			return nil, err
		}
		repos, err := db.RQ.ListRepos(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		d["P"], d["Repos"], d["Title"] = p, repos, p.Name
		d["Templates"], _ = db.RQ.ListTemplates(ctx)
		d["GitHub"] = h.D.Projects.GH != nil
		d["Template"] = ""
		if p.DefaultTemplateID.Valid {
			if t, err := db.RQ.GetTemplate(ctx, p.DefaultTemplateID.Int64); err == nil {
				d["Template"] = t.Name
			}
		}
	case "setup":
		d["Title"] = "Setup"
		app, have := h.D.Setup.Current()
		d["App"], d["HaveApp"] = app, have
		d["Active"] = h.D.Projects.GH != nil
		d["State"] = h.D.Setup.NewState()
		d["AppName"] = h.D.Setup.AppName
		m, err := h.D.Setup.Manifest(h.D.Setup.AppName)
		d["Manifest"], d["ManifestErr"] = string(m), errString(err)
		d["Created"], d["Error"] = r.URL.Query().Get("created") == "1", r.URL.Query().Get("error")
	case "settings":
		d["Projects"], _ = db.RQ.ListProjects(ctx)
		d["Templates"], _ = db.RQ.ListTemplates(ctx)
		d["Templates"], _ = db.RQ.ListTemplates(ctx)
		d["VAPID"], d["Title"] = h.D.Push.Keys.Public, "Settings"
	}
	return d, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func parentDir(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i+1]
	}
	if p == "" {
		return ""
	}
	return "/"
}

// tree builds the nested backlog with lock badges.
func (h *Handler) tree(ctx context.Context, slug string, all bool) ([]*node, error) {
	db := h.D.Fleet.DB
	p, err := db.RQ.GetProjectBySlug(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("%w: project %s", errs.NotFound, slug)
	}
	rows, err := db.RQ.ProjectTree(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	active, _ := db.RQ.ListRuns(ctx, dbgen.ListRunsParams{State: "active", Lim: 1000})
	locks := map[int64]string{}
	for _, r := range active {
		locks[r.Run.TaskID] = map[bool]string{true: r.RunnerName, false: "queued"}[r.RunnerName != ""]
	}
	attn := h.attention(ctx, slug)
	byID := map[int64]*node{}
	var roots []*node
	lockOf := map[int64]string{}
	for _, t := range rows {
		n := &node{TaskNode: proto.TaskNode{Sqid: t.Sqid, Title: t.Title, State: t.State, Type: string(t.TaskType), Target: t.TargetDate.String, Depth: t.Depth, Unmet: t.Unmet, Children: t.Children},
			Attention: attn[t.Sqid] && t.State != "done" && t.State != "cancelled"}
		if t.TaskType.Planning() {
			if full, err := db.RQ.GetTask(ctx, t.ID); err == nil {
				n.Done, n.Total, n.Slip, _ = h.D.Stack.Rollup(ctx, full)
			}
		}
		lock := locks[t.ID]
		if lock == "" && t.ParentID.Valid {
			lock = lockOf[t.ParentID.Int64]
		}
		lockOf[t.ID], n.LockedBy = lock, lock
		byID[t.ID] = n
		if !all && (t.State == "done" || t.State == "cancelled") {
			continue
		}
		if parent, ok := byID[t.ParentID.Int64]; ok && t.ParentID.Valid {
			n.Parent = parent.Sqid
			parent.Kids = append(parent.Kids, n)
		} else if !t.ParentID.Valid {
			roots = append(roots, n)
		}
	}
	return roots, nil
}

// attention is the set of task sqids that need the owner: an open question on the task,
// or the task's most recent run failed, is blocked or waits for input.
func (h *Handler) attention(ctx context.Context, project string) map[string]bool {
	out := map[string]bool{}
	if qs, err := h.D.Runs.Questions(ctx, false); err == nil {
		for _, q := range qs {
			if q.Project == project {
				out[q.Task] = true
			}
		}
	}
	runs, _ := h.D.Fleet.DB.RQ.ListRuns(ctx, dbgen.ListRunsParams{Project: project, Lim: 500})
	seen := map[string]bool{}
	for _, r := range runs { // newest first: only the latest run per task counts
		if seen[r.TaskSqid] {
			continue
		}
		seen[r.TaskSqid] = true
		switch r.Run.State {
		case "failed", "blocked", "needs_input":
			out[r.TaskSqid] = true
		}
	}
	return out
}

// outlineFromPayload turns a plan proposal into the split sheet's outline text.
func outlineFromPayload(p json.RawMessage) string {
	var prop struct {
		Children []proto.SplitNode `json:"children"`
	}
	json.Unmarshal(p, &prop)
	var b strings.Builder
	var walk func([]proto.SplitNode, string)
	walk = func(ns []proto.SplitNode, ind string) {
		for _, n := range ns {
			b.WriteString(ind + "- " + n.Title + "\n")
			walk(n.Children, ind+"  ")
		}
	}
	walk(prop.Children, "")
	return b.String()
}

// --- actions ---------------------------------------------------------------------------

// act handles POST /ui/act/{action}: Datastar sends its signals as JSON.
func (h *Handler) act(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var sig map[string]any
	json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&sig)
	str := func(k string) string { v, _ := sig[k].(string); return strings.TrimSpace(v) }
	flag := func(k string) bool { v, _ := sig[k].(bool); return v }
	q := r.URL.Query()
	sqid := q.Get("sqid")
	id, _ := strconv.ParseInt(q.Get("id"), 10, 64)
	var msg string
	var err error
	nav := ""
	var reset map[string]any // signals to clear on success
	switch r.PathValue("action") {
	case "task-new":
		var t dbgen.Task
		pos := stack.Position{First: q.Get("first") == "1", After: q.Get("after")}
		t, err = h.D.Stack.Create(ctx, stack.CreateParams{Project: q.Get("project"), Parent: q.Get("parent"), Title: str("title"), Prompt: str("prompt"), Type: str("ttype"), TargetDate: str("ttarget"), Pos: pos, Actor: stack.User})
		if err == nil {
			msg = "created " + t.Sqid.String
			reset = map[string]any{"title": "", "prompt": "", "queue": false, "ttarget": ""}
			if q.Get("parent") == "" { // the backlog sheet's type picker (subtasks inherit instead)
				reset["ttype"] = "code"
			}
			if flag("queue") {
				if _, qerr := h.D.Stack.Queue(ctx, t.Sqid.String); qerr != nil {
					msg += " (not queued: " + qerr.Error() + ")"
				} else {
					msg += " and queued"
				}
			}
		}
	case "task-edit":
		title, prompt := str("etitle"), str("eprompt")
		if _, err = h.D.Stack.Edit(ctx, sqid, &title, &prompt, stack.User); err == nil && str("etype") != "" {
			_, err = h.D.Stack.SetType(ctx, sqid, str("etype"), stack.User)
		}
		if err == nil {
			_, err = h.D.Stack.SetTargetDate(ctx, sqid, str("etarget"), stack.User)
		}
		msg = "saved"
	case "task-split":
		var nodes []stack.SplitNode
		if nodes, err = stack.ParseOutline(str("outline")); err == nil {
			_, err = h.D.Stack.Split(ctx, sqid, nodes, flag("chain"), flag("copy"), stack.User)
			msg = fmt.Sprintf("split into %d subtasks", len(nodes))
			reset = map[string]any{"outline": ""}
		}
	case "task-plan":
		_, err = h.D.Plans.Queue(ctx, sqid, str("guidance"))
		msg = "planning… the proposal will arrive in Inbox"
	case "task-queue":
		_, err = h.D.Stack.Queue(ctx, sqid)
		msg = "queued"
	case "task-complete", "task-reopen", "task-cancel":
		op := strings.TrimPrefix(r.PathValue("action"), "task-")
		switch op {
		case "complete":
			err = h.D.Stack.Complete(ctx, sqid, "", stack.User)
		case "reopen":
			err = h.D.Stack.Reopen(ctx, sqid, stack.User)
		case "cancel":
			err = h.D.Stack.Cancel(ctx, sqid, "", stack.User)
		}
		msg = op + "d"
	case "task-delete":
		t, terr := h.D.Stack.Task(ctx, h.D.Fleet.DB.RQ, sqid)
		if err = terr; err == nil {
			p, _ := h.D.Fleet.DB.RQ.GetProject(ctx, t.ProjectID)
			err = h.D.Stack.Delete(ctx, sqid, stack.User)
			msg, nav = "deleted", "/backlog?project="+url.QueryEscape(p.Slug)
		}
	case "task-needs":
		err = h.D.Stack.NeedsAdd(ctx, sqid, str("precursor"), stack.User)
		msg = "precursor added"
	case "answer":
		err = h.D.Runs.Answer(ctx, id, q.Get("value"), str(fmt.Sprintf("ans%d", id)))
		msg = "answered"
		if q.Get("value") == "edit" { // plan proposal: open the split sheet with it
			if qv, qerr := h.D.Runs.Question(ctx, id); qerr == nil {
				nav = fmt.Sprintf("/task/%s?split=%d#split", qv.Task, id)
			}
		}
	case "pr":
		switch q.Get("op") {
		case "approve":
			err = h.D.PRs.Approve(ctx, id)
		case "request-changes":
			err = h.D.PRs.RequestChanges(ctx, id, str(fmt.Sprintf("pr%d", id)))
		case "close":
			err = h.D.PRs.Close(ctx, id)
		}
		msg = "PR: " + q.Get("op")
	case "run-cancel":
		err = h.D.Runs.Cancel(ctx, id)
		msg = "cancelling"
	case "runner":
		name := q.Get("name")
		switch q.Get("op") {
		case "cordon":
			err = h.D.Fleet.Cordon(ctx, name, true)
		case "uncordon":
			err = h.D.Fleet.Cordon(ctx, name, false)
		case "drain":
			err = h.D.Fleet.Drain(ctx, name)
		case "terminate":
			err = h.D.Fleet.Terminate(ctx, name)
		}
		msg = name + ": " + q.Get("op")
	case "fleet-launch":
		_, err = h.D.Fleet.Launch(ctx, q.Get("template"))
		msg = "launching a " + q.Get("template") + " runner"
	case "project-new":
		slug := str("slug")
		pp := projectsParams(str("ptemplate"))
		if n := str("pname"); n != "" {
			pp.Name = &n
		}
		if g := str("pgoals"); g != "" {
			pp.Description = &g
		}
		if _, err = h.D.Projects.Create(ctx, slug, pp); err != nil {
			break
		}
		msg, nav = "project created", "/project/"+url.PathEscape(slug)
		var failed []string
		for _, ref := range strings.FieldsFunc(str("prepos"), func(r rune) bool { return r == ',' || r == '\n' || r == ' ' }) {
			if _, lerr := h.D.Projects.LinkRepo(ctx, slug, ref, ""); lerr != nil {
				failed = append(failed, ref+": "+lerr.Error())
			}
		}
		if len(failed) > 0 {
			msg = "project created; not linked: " + strings.Join(failed, "; ")
		}
		reset = map[string]any{"slug": "", "pname": "", "pgoals": "", "prepos": "", "ptemplate": ""}
	case "project-save":
		slug := q.Get("slug")
		name, goals, tmpl := str("pname"), str("pgoals"), str("ptemplate")
		am, aq := flag("pautomerge"), flag("pautoqueue")
		_, err = h.D.Projects.Update(ctx, slug, projects.ProjectParams{Name: &name, Description: &goals, Template: &tmpl, AutoMerge: &am, AutoQueue: &aq})
		msg = "saved " + slug
	case "repo-link":
		_, err = h.D.Projects.LinkRepo(ctx, q.Get("slug"), str("repoadd"), "")
		msg, reset = "repo linked", map[string]any{"repoadd": ""}
	case "repo-unlink":
		err = h.D.Projects.UnlinkRepo(ctx, q.Get("slug"), q.Get("repo"))
		msg = "repo unlinked"
	default:
		err = fmt.Errorf("unknown action")
	}
	if err != nil {
		toast(w, err.Error(), true)
		return
	}
	toast(w, msg, false)
	if reset != nil {
		mergeSignals(w, reset)
	}
	if nav != "" {
		executeScript(w, fmt.Sprintf("window.location.assign(%q)", nav))
	}
}

// --- helpers ---------------------------------------------------------------------------

func ago(ts string) string {
	if ts == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ts
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1fh ago", d.Hours())
	}
	return t.Format("2 Jan")
}

// sparkline renders values (0-100) as an inline SVG polyline.
func sparkline(vals []float64) template.HTML {
	if len(vals) < 2 {
		return ""
	}
	const w, h = 120.0, 24.0
	var pts []string
	for i, v := range vals {
		x := float64(i) / float64(len(vals)-1) * w
		y := h - (v/100)*h
		pts = append(pts, fmt.Sprintf("%.1f,%.1f", x, y))
	}
	return template.HTML(fmt.Sprintf(`<svg class="spark" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none"><polyline points="%s"/></svg>`, w, h, strings.Join(pts, " ")))
}

type diffFile struct {
	Name     string
	Add, Del int
	Lines    []diffLine
}

type diffLine struct {
	Kind, Text string // add | del | hunk | ctx | meta
}

// parseDiff splits a unified diff into files for rendering.
func parseDiff(s string) []diffFile {
	var files []diffFile
	var cur *diffFile
	for _, line := range strings.Split(s, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			files = append(files, diffFile{})
			cur = &files[len(files)-1]
			parts := strings.SplitN(line, " b/", 2)
			if len(parts) == 2 {
				cur.Name = parts[1]
			}
		case cur == nil:
		case strings.HasPrefix(line, "@@"):
			cur.Lines = append(cur.Lines, diffLine{"hunk", line})
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"), strings.HasPrefix(line, "index "), strings.HasPrefix(line, "new file"), strings.HasPrefix(line, "deleted file"):
		case strings.HasPrefix(line, "+"):
			cur.Add++
			cur.Lines = append(cur.Lines, diffLine{"add", line[1:]})
		case strings.HasPrefix(line, "-"):
			cur.Del++
			cur.Lines = append(cur.Lines, diffLine{"del", line[1:]})
		case strings.HasPrefix(line, " "):
			cur.Lines = append(cur.Lines, diffLine{"ctx", line[1:]})
		}
	}
	return files
}

// recentProjects splits projects into the n most recently updated and the rest (both in
// recency order, ties broken by slug).
func recentProjects(all []dbgen.Project, n int) (recent, rest []dbgen.Project) {
	sorted := append([]dbgen.Project(nil), all...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].UpdatedAt > sorted[j].UpdatedAt })
	if len(sorted) <= n {
		return sorted, nil
	}
	return sorted[:n], sorted[n:]
}

// githubCallback is where GitHub sends the owner's browser after they confirm the App.
func (h *Handler) githubCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if _, err := h.D.Setup.Exchange(r.Context(), q.Get("code"), q.Get("state")); err != nil {
		http.Redirect(w, r, "/setup?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/setup?created=1", http.StatusSeeOther)
}

// termUsable reports whether a runner in this state can host a terminal: it is up and
// enrolled (launching, enrolling, offline and terminated runners have no daemon to talk to).
func termUsable(state string) bool {
	switch state {
	case "provisioning", "idle", "busy", "cordoned", "draining":
		return true
	}
	return false
}

// termPage serves the fullscreen runner terminal. It has its own minimal document, with no
// navigation, so it can fill a phone screen; the page script does the rest.
func (h *Handler) termPage(w http.ResponseWriter, r *http.Request) {
	rn, err := h.D.Fleet.DB.RQ.GetRunnerByName(r.Context(), r.PathValue("name"))
	if err != nil {
		http.Error(w, "runner not found", http.StatusNotFound)
		return
	}
	var b bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&b, "termpage", map[string]any{"Name": rn.Name, "State": rn.State, "Usable": termUsable(rn.State)}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b.Bytes())
}

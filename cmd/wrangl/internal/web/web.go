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
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/gh"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/ghsetup"
	"gopkg.in/yaml.v3"
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

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/agentdef"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/api"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/agents"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/projects"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbtypes"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/stack"
)

//go:embed static templates
var assets embed.FS

// Handler serves the PWA.
type Handler struct {
	D    api.Deps
	tmpl *template.Template
	css  string // chroma styles (light + dark)

	sessions sync.Map // page session id -> *liveSession
	health   sync.Map // lowercase login -> cached App health (see appHealth)
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
		"glyph":  func(s string) string { return glyph[s] },
		"ago":    ago,
		"md":     markdown,
		"pct":    func(v float64) string { return fmt.Sprintf("%.0f%%", v) },
		"spark":  sparkline,
		"json":   func(v any) template.JS { b, _ := json.Marshal(v); return template.JS(b) },
		"q":      url.QueryEscape,
		"add":    func(a, b int) int { return a + b },
		"int":    func(v int64) int { return int(v) },
		"memgib": func(mib int64) string { return fmt.Sprintf("%g GiB", float64(mib)/1024) },
		"inc":    func(i int) int { return i + 1 },
		"usd":    func(v float64) string { return fmt.Sprintf("$%.2f", v) },
		"usdh":   func(v float64) string { return fmt.Sprintf("$%.3f", v) },
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
	for _, f := range []string{"sw.js", "manifest.webmanifest"} {
		f := f
		mux.HandleFunc("GET /"+f, func(w http.ResponseWriter, r *http.Request) {
			if f == "manifest.webmanifest" {
				w.Header().Set("Content-Type", "application/manifest+json")
			}
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
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/tasks", http.StatusFound) })
	for path, view := range map[string]string{
		"/tasks": "tasks", "/task/{sqid}": "task", "/runners": "runners", "/runner/{name}": "runner",
		"/logs": "logs", "/run/{id}": "run", "/run/{id}/diff": "diff", "/run/{id}/files": "files",
		"/inbox": "inbox", "/settings": "settings", "/settings/templates/{name}": "template", "/project/{slug}": "project", "/setup": "setup",
	} {
		view := view
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) { h.page(w, r, view) })
	}
	mux.HandleFunc("GET /backlog", func(w http.ResponseWriter, r *http.Request) { // the old name of /tasks
		to := "/tasks"
		if r.URL.RawQuery != "" {
			to += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, to+"#backlog", http.StatusFound)
	})
	mux.HandleFunc("GET /runs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/logs", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /runner/{name}/term", h.termPage)
	mux.HandleFunc("GET /setup/github/callback", h.githubCallback)
	mux.HandleFunc("GET /ui/live", h.live)
	mux.HandleFunc("GET /more", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/settings", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /ui/detail", h.detail)
	mux.HandleFunc("POST /ui/expand", h.expand)
	mux.HandleFunc("GET /deliverable/{id}", h.deliverable)
	mux.HandleFunc("GET /deliverable/{id}/proto", h.deliverable)
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
	if d.Kind == "prototype_html" {
		if strings.HasSuffix(r.URL.Path, "/proto") {
			// A committed static prototype: it may run its own inline script and style, but it
			// has an opaque origin (sandbox without allow-same-origin), no network, no forms,
			// no popups, no navigation of the app, and can only load data: images and fonts.
			w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; media-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'; sandbox allow-scripts")
			w.Header().Set("Referrer-Policy", "no-referrer")
		} else { // the plain URL shows the source, never runs it
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
	} else if strings.HasSuffix(r.URL.Path, "/proto") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=3600")
	io.Copy(w, f)
}

// deliverableViews loads deliverables for the task or run page, rendering reports.
func (h *Handler) deliverableViews(ctx context.Context, rows []dbgen.Deliverable) []deliverableView {
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
	return dv
}

// deliverableView is a deliverable ready for the task page.
type deliverableView struct {
	dbgen.Deliverable
	Report  template.HTML // rendered markdown, for reports (without the Sources section)
	Sources template.HTML // the report's Sources section, shown as a panel
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
	if _, ok := map[string]bool{"tasks": true, "task": true, "runners": true, "runner": true, "logs": true, "run": true, "inbox": true, "settings": true, "project": true}[view]; !ok {
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
		if sq := sess.get(); view == "tasks" && sq != "" {
			if card, err := h.taskPart(r.Context(), sq, "main-task"); err == nil {
				if roots, ok := data["Roots"].([]*node); ok {
					all := roots
					for _, k := range []string{"Progress", "Queue"} {
						if es, ok := data[k].([]*runEntry); ok {
							for _, e := range es {
								all = append(all, e.Node)
							}
						}
					}
					attachDetail(all, sq, card)
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

// mergeFragments writes a Datastar patch-elements event (one data line per HTML line): the
// elements are morphed into the page by id.
func mergeFragments(w io.Writer, html string) {
	io.WriteString(w, "event: datastar-patch-elements\n")
	for _, line := range strings.Split(html, "\n") {
		fmt.Fprintf(w, "data: elements %s\n", line)
	}
	io.WriteString(w, "\n")
}

// mergeSignals writes a Datastar patch-signals event.
func mergeSignals(w io.Writer, signals map[string]any) {
	b, _ := json.Marshal(signals)
	fmt.Fprintf(w, "event: datastar-patch-signals\ndata: signals %s\n\n", b)
}

// executeScript runs js in the page: Datastar has no script event, so the script is appended
// to <body> and removes itself once it has run.
func executeScript(w io.Writer, js string) {
	io.WriteString(w, "event: datastar-patch-elements\ndata: selector body\ndata: mode append\n")
	fmt.Fprintf(w, "data: elements <script data-effect=\"el.remove()\">%s</script>\n\n", strings.ReplaceAll(js, "\n", " "))
}

// goLater sends the browser to the page that fixes a GitHub App problem, after the toast has
// been readable for a moment (the toast's link is there if the redirect is not wanted).
func goLater(w http.ResponseWriter, to string) {
	executeScript(w, fmt.Sprintf("setTimeout(() => window.location.assign(%q), 1800)", to))
}

// toastLink is an error toast with a link to the next step.
func toastLink(w http.ResponseWriter, msg, href, label string) {
	w.Header().Set("Content-Type", "text/event-stream")
	mergeFragments(w, fmt.Sprintf(`<div id="toast" class="toast err show">%s <a href="%s">%s</a></div>`,
		template.HTMLEscapeString(msg), template.HTMLEscapeString(href), template.HTMLEscapeString(label)))
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
	Crumbs    []crumb       // "Now" view: ancestors, root first
	Mockups   int           // deliverables (mockups, prototypes, reports) handed back on this task
	Detail    template.HTML // the expanded task's card, when this page has it open
	Kids      []*node
}

// crumb is one ancestor in a breadcrumb.
type crumb struct{ Sqid, Title string }

// nowLeaves lists leaf tasks whose state is in_progress (in_review ones belong in the inbox),
// each with its ancestors as a breadcrumb. within, if non-zero, limits it to that task's subtree.
func nowLeaves(rows []dbgen.ProjectTreeRow, within int64) []*node {
	byID := map[int64]dbgen.ProjectTreeRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	var out []*node
	for _, r := range rows {
		if r.State != "in_progress" || r.Children != 0 {
			continue
		}
		var up []crumb
		inside := within == 0
		for p := r; p.ParentID.Valid; {
			par, ok := byID[p.ParentID.Int64]
			if !ok {
				break
			}
			if par.ID == within {
				inside = true
			}
			up = append([]crumb{{par.Sqid, par.Title}}, up...)
			p = par
		}
		if !inside {
			continue
		}
		n := &node{TaskNode: proto.TaskNode{Sqid: r.Sqid, Title: r.Title, State: r.State, Type: string(r.TaskType), Depth: r.Depth, Children: r.Children}, Crumbs: up}
		if len(up) > 0 {
			n.Parent = up[len(up)-1].Sqid
		}
		out = append(out, n)
	}
	return out
}

// runEntry is an active run shown in the In progress or Queue section: its root task (with the
// subtree beneath it) is taken out of the backlog tree.
type runEntry struct {
	Node         *node
	RunID        int64
	Kind, State  string
	Runner       string
	Current      string // sqid of the leaf being worked
	CurrentTitle string
	Rank         sql.NullFloat64 // owner-chosen queue_rank (Queue only)
	Crumb        string          // "parent > " for a queued task that is not a root
}

// taskSections is what the Tasks page lists: running, queued and the rest.
type taskSections struct {
	Progress, Queue []*runEntry
	Backlog         []*node
	Now             []*node // in-progress leaf tasks, with breadcrumbs
}

func (h *Handler) data(ctx context.Context, view string, r *http.Request) (map[string]any, error) {
	db := h.D.Fleet.DB
	d := map[string]any{}
	switch view {
	case "tasks":
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
		d["Project"], d["Title"] = slug, "Tasks"
		d["Types"] = dbtypes.TaskTypes
		d["All"] = r.URL.Query().Get("all") == "1"
		if slug != "" {
			secs, err := h.tree(ctx, slug, d["All"].(bool))
			if err != nil {
				return nil, err
			}
			d["Roots"], d["Progress"], d["Queue"] = secs.Backlog, secs.Progress, secs.Queue
			d["Now"], d["NowOnly"] = secs.Now, r.URL.Query().Get("now") == "1"
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
			dv := h.deliverableViews(ctx, rows)
			d["Deliverables"] = dv
		}
		if t.ParentID.Valid {
			if pt, err := db.RQ.GetTask(ctx, t.ParentID.Int64); err == nil {
				d["Parent"] = pt
			}
		}
		kids, _ := db.RQ.ListChildren(ctx, sql.NullInt64{Int64: t.ID, Valid: true})
		d["Kids"] = kids
		if len(kids) > 0 { // a container: what is being worked on beneath it right now
			if rows, err := db.RQ.ProjectTree(ctx, t.ProjectID); err == nil {
				d["Now"] = nowLeaves(rows, t.ID)
			}
		}
		if cp, err := db.RQ.LatestTaskCheckpoint(ctx, t.ID); err == nil {
			d["Checkpoint"] = checkpointLinks(cp)
		}
		// Queue is offered on every live task; if nothing is runnable the refusal says why (e.g.
		// subtasks still in review).
		d["CanQueue"] = t.State != "done" && t.State != "cancelled"
		if h.D.Agents != nil {
			d["Thread"], d["Agents"] = h.threadView(ctx, t.ID)
		}
		// The live PTY: the in-progress run working on this task (or its subtasks). The PTY is the
		// run's own session on whichever runner hosts it, so it is keyed by the run, not the runner.
		if runs, err := db.RQ.ListRuns(ctx, dbgen.ListRunsParams{State: "active", Lim: 1000}); err == nil {
			for _, x := range runs {
				if (x.TaskSqid == sqid || x.CurrentSqid == sqid) && x.Run.RunnerID.Valid && x.Run.State != "queued" {
					d["LiveRun"] = x.Run.ID
					break
				}
			}
		}
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
			Used    int64 // agent sessions in use (active runs); the limit is T.MaxSessions
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
			c.Used, _ = db.RQ.CountActiveRunsForRunner(ctx, sql.NullInt64{Int64: x.Runner.ID, Valid: true})
			cards = append(cards, c)
		}
		// Machines the fleet doesn't launch (the controller host) are permanent: pin them first.
		sort.SliceStable(cards, func(i, j int) bool {
			return cards[i].R.Launched == 0 && cards[j].R.Launched != 0
		})
		d["Cards"] = cards
		if h.D.Costs != nil {
			snap := h.D.Costs.Cached()
			known := map[string]bool{}
			for _, c := range cards {
				if c.R.InstanceID.Valid && c.R.State != "terminated" {
					known[c.R.InstanceID.String] = true
				}
			}
			d["Spend"], d["SpendAsOf"], d["SpendErrors"] = snap.Summary, snap.AsOf, snap.Errors
			others := api.OtherInstances(snap, known)
			d["Others"] = others
			var otherHourly float64
			for _, o := range others {
				if o.State == "running" && o.PriceKnown {
					otherHourly += o.HourlyUSD
				}
			}
			d["OtherHourly"] = otherHourly
			d["Top"] = snap.Top
			d["Templates2"], _ = db.RQ.ListTemplates(ctx)
		}
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
	case "logs":
		rows, err := h.logRows(ctx, reqValue(r, "kind"))
		if err != nil {
			return nil, err
		}
		d["Rows"], d["Kind"], d["Title"] = rows, reqValue(r, "kind"), "Logs"
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
			if h.D.Deliverables != nil {
				rows, _ := db.RQ.ListDeliverablesForRun(ctx, sql.NullInt64{Int64: id, Valid: true})
				d["Deliverables"] = h.deliverableViews(ctx, rows)
			}
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
		d["NotesRepo"], _ = db.RQ.GetNotesRepoID(ctx, p.ID)
		d["Templates"], _ = db.RQ.ListTemplates(ctx)
		d["Limits"], _ = h.D.Projects.Limits(ctx, p.Slug)
		d["GitHub"] = h.D.Projects.GH != nil
		d["Pick"] = h.repoChoices(ctx, p.Slug, repos)
		d["Template"] = ""
		if p.DefaultTemplateID.Valid {
			if t, err := db.RQ.GetTemplate(ctx, p.DefaultTemplateID.Int64); err == nil {
				d["Template"] = t.Name
			}
		}
	case "setup":
		d["Title"] = "GitHub accounts"
		qv := r.URL.Query()
		back := ghsetup.SafeBack(qv.Get("back"))
		accts, _ := h.D.Setup.Accounts(ctx)
		var views []acctView
		seen := false
		for _, ac := range accts {
			v := acctView{Account: ac, AppName: ghsetup.AppName(ac.Login)}
			if ac.App != nil { // re-authorizing makes a new App: names are unique across GitHub
				v.AppName = ghsetup.FreshAppName(ac.Login)
			}
			v.State = h.D.Setup.NewState(ac.Login, back)
			m, err := h.D.Setup.Manifest(v.AppName)
			v.Manifest, v.ManifestErr = string(m), errString(err)
			v.Status, v.StatusText, v.NRepos = h.appHealth(ctx, ac)
			v.Auto = ac.App == nil && strings.EqualFold(qv.Get("create"), ac.Login) || ac.App == nil && qv.Get("create") == "1" && strings.EqualFold(qv.Get("account"), ac.Login)
			v.JustCreated = ac.App != nil && qv.Get("role") != "reviewer" && strings.EqualFold(qv.Get("created"), ac.Login) && v.Status == "noinstall"
			if ac.Reviewer == nil {
				v.RName = ghsetup.ReviewerName(ac.Login)
				v.RState = h.D.Setup.NewRoleState(ac.Login, back, "reviewer")
				m, err := h.D.Setup.ManifestFor(v.RName, "reviewer")
				v.RManifest, v.RManifestErr = string(m), errString(err)
			}
			v.Focus = strings.EqualFold(qv.Get("account"), ac.Login)
			seen = seen || v.Focus
			views = append(views, v)
		}
		if h.D.Secrets != nil {
			defs, _ := db.RQ.ListAgentDefs(ctx)
			d["Creds"] = agentdef.Credentials(defs, h.D.Secrets.Has)
		}
		d["Accounts"], d["Created"], d["Error"] = views, qv.Get("created"), qv.Get("error")
		d["CreatedRole"] = qv.Get("role")
		d["Repo"], d["Back"] = qv.Get("repo"), back
		if f := qv.Get("account"); f != "" && !seen {
			d["Suggest"] = f // an owner we could not add (GitHub did not know it): prefill the add box
		}
	case "template":
		t, err := h.D.Fleet.Template(ctx, reqValue(r, "name"))
		if err != nil {
			return nil, err
		}
		d["T"], d["Title"] = t, "Template "+t.Name
		var types []string
		json.Unmarshal([]byte(t.InstanceTypes), &types)
		ticked := map[string]bool{}
		for _, x := range types {
			ticked[x] = true
		}
		d["Ticked"], d["TypesList"], d["TypesCSV"] = ticked, types, strings.Join(types, ",")
		d["SessStr"] = strconv.FormatInt(t.MaxSessions, 10)
		d["MaxStr"], d["IdleStr"] = strconv.FormatInt(t.MaxRunners, 10), shortDuration(time.Duration(t.IdleTimeoutS)*time.Second)
		d["VcpuStr"], d["RamStr"], d["DiskStr"] = strconv.FormatInt(t.MinVcpu, 10), fmt.Sprintf("%gGiB", float64(t.MinRamMib)/1024), fmt.Sprintf("%dGiB", t.MinDiskGib)
		d["ImgMode"], d["AmiStr"] = "al", ""
		if strings.HasPrefix(t.Ami, "ami-") {
			d["ImgMode"], d["AmiStr"] = "ami", t.Ami
		}
		var agent struct {
			Start []string `json:"start"`
		}
		json.Unmarshal([]byte(t.AgentJson), &agent)
		d["AgentSummary"] = "no agent"
		if len(agent.Start) > 0 {
			d["AgentSummary"] = "runs " + agent.Start[0]
		}
		d["HasAWS"] = h.D.Fleet.Prov != nil
		if h.D.Costs != nil {
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			cat, err := h.D.Costs.Catalogue(cctx)
			cancel()
			d["Catalogue"], d["CatalogueErr"], d["Region"] = cat, errString(err), h.D.Costs.Home
		}
	case "settings":
		d["GHAccounts"], _ = h.D.Setup.Accounts(ctx)
		d["Projects"], _ = db.RQ.ListProjects(ctx)
		d["Templates"], _ = db.RQ.ListTemplates(ctx)
		d["CAgents"], _ = db.RQ.ListControllerAgents(ctx)
		d["VAPID"], d["Title"] = h.D.Push.Keys.Public, "Settings"
	}
	return d, nil
}

// checkpointView links a task to the commit its run checkpointed it at.
type checkpointView struct{ Short, Commit, Compare string }

// checkpointLinks builds the GitHub commit and compare URLs. The compare base is the previous
// task's checkpoint in the same run, or the default branch for the first task.
func checkpointLinks(cp dbgen.LatestTaskCheckpointRow) checkpointView {
	repo := "https://github.com/" + cp.Owner + "/" + cp.Repo
	base := cp.PrevSha
	if base == "" {
		base = cp.DefaultBranch
	}
	short := cp.Sha
	if len(short) > 7 {
		short = short[:7]
	}
	return checkpointView{Short: short, Commit: repo + "/commit/" + cp.Sha, Compare: repo + "/compare/" + base + "..." + cp.Sha}
}

// threadMsg is one message of a task's conversation with a controller agent.
type threadMsg struct {
	ID                      int64
	Role, Status, Agent, At string
	Body                    template.HTML
	Props                   []agents.Proposal
}

// threadView loads a task's thread and the agents that can be asked.
func (h *Handler) threadView(ctx context.Context, taskID int64) ([]threadMsg, []dbgen.ControllerAgent) {
	db := h.D.Agents.DB
	ags, _ := db.RQ.ListControllerAgents(ctx)
	names := map[int64]string{}
	var usable []dbgen.ControllerAgent
	for _, a := range ags {
		names[a.ID] = a.Name
		if a.Enabled == 1 {
			usable = append(usable, a)
		}
	}
	rows, _ := db.RQ.ListThread(ctx, taskID)
	var out []threadMsg
	for _, m := range rows {
		tm := threadMsg{ID: m.ID, Props: agents.ParseProposals(m.Proposals), Role: m.Role, Status: m.Status, At: m.At, Agent: names[m.AgentID.Int64]}
		if m.Role == "agent" {
			tm.Body = markdown(m.Body)
		} else {
			tm.Body = template.HTML("<p>" + strings.ReplaceAll(template.HTMLEscapeString(m.Body), "\n", "<br>") + "</p>")
		}
		out = append(out, tm)
	}
	return out, usable
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
func (h *Handler) tree(ctx context.Context, slug string, all bool) (taskSections, error) {
	var secs taskSections
	db := h.D.Fleet.DB
	p, err := db.RQ.GetProjectBySlug(ctx, slug)
	if err != nil {
		return secs, fmt.Errorf("%w: project %s", errs.NotFound, slug)
	}
	rows, err := db.RQ.ProjectTree(ctx, p.ID)
	if err != nil {
		return secs, err
	}
	active, _ := db.RQ.ListRuns(ctx, dbgen.ListRunsParams{State: "active", Lim: 1000})
	locks := map[int64]string{}
	for _, r := range active {
		locks[r.Run.TaskID] = map[bool]string{true: r.RunnerName, false: "queued"}[r.RunnerName != ""]
	}
	secs.Now = nowLeaves(rows, 0)
	attn := h.attention(ctx, slug)
	mockups := map[int64]int{}
	if cs, err := db.RQ.CountDeliverablesForTasks(ctx, p.ID); err == nil {
		for _, c := range cs {
			mockups[c.TaskID] = int(c.N)
		}
	}
	byID := map[int64]*node{}
	bySqid := map[string]*node{}
	order := map[string]int{} // tree position, for ordering the queue
	var roots []*node
	lockOf := map[int64]string{}
	for _, t := range rows {
		n := &node{TaskNode: proto.TaskNode{Sqid: t.Sqid, Title: t.Title, State: t.State, Type: string(t.TaskType), Target: t.TargetDate.String, Depth: t.Depth, Unmet: t.Unmet, Children: t.Children},
			Attention: attn[t.Sqid] && t.State != "done" && t.State != "cancelled", Mockups: mockups[t.ID]}
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
		bySqid[t.Sqid] = n
		order[t.Sqid] = len(order)
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
	// Active runs leave the backlog: queued ones go to Queue, assigned/running ones to In progress.
	runs, _ := db.RQ.ListRuns(ctx, dbgen.ListRunsParams{State: "active", Project: slug, Lim: 1000})
	taken := map[*node]bool{}
	for i := len(runs) - 1; i >= 0; i-- { // oldest first
		r := runs[i]
		n := bySqid[r.TaskSqid]
		if n == nil || taken[n] {
			continue
		}
		taken[n] = true
		e := &runEntry{Node: n, RunID: r.Run.ID, Rank: r.Run.QueueRank, Kind: r.Run.Kind, State: r.Run.State, Runner: r.RunnerName, Current: r.CurrentSqid}
		if c := bySqid[r.CurrentSqid]; c != nil {
			e.CurrentTitle = c.Title
		}
		if n.Parent != "" {
			if p := bySqid[n.Parent]; p != nil {
				e.Crumb = p.Title
			}
		}
		if r.Run.State == "queued" {
			secs.Queue = append(secs.Queue, e)
		} else {
			secs.Progress = append(secs.Progress, e)
		}
	}
	// Owner-ranked runs first (by queue_rank), then tree order (project priority/rank), then FIFO.
	sort.SliceStable(secs.Queue, func(a, b int) bool {
		ra, rb := secs.Queue[a].Rank, secs.Queue[b].Rank
		if ra.Valid != rb.Valid {
			return ra.Valid
		}
		if ra.Valid && ra.Float64 != rb.Float64 {
			return ra.Float64 < rb.Float64
		}
		return order[secs.Queue[a].Node.Sqid] < order[secs.Queue[b].Node.Sqid]
	})
	var strip func(ns []*node) []*node
	strip = func(ns []*node) []*node {
		var out []*node
		for _, n := range ns {
			if taken[n] {
				continue
			}
			n.Kids = strip(n.Kids)
			out = append(out, n)
		}
		return out
	}
	secs.Backlog = strip(roots)
	return secs, nil
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
	case "task-ask":
		if h.D.Agents == nil {
			err = errs.Invalid
			break
		}
		question, field := str("askq"), "askq"
		if q.Get("reply") == "1" {
			question, field = str("replyq"), "replyq"
		}
		if _, err = h.D.Agents.Ask(ctx, sqid, str("askagent"), question); err == nil {
			msg = "asked; the answer will appear on the task"
			reset = map[string]any{field: ""}
		}
	case "proposal-apply":
		n, _ := strconv.Atoi(q.Get("n"))
		if err = h.D.Agents.Apply(ctx, id, n); err == nil {
			msg = "done"
		}
	case "account-check": // drop the cached health so the page re-checks, and reload it
		h.health.Delete(strings.ToLower(q.Get("login")))
		msg, nav = "checking…", "/setup?account="+url.QueryEscape(q.Get("login"))
	case "agent-save":
		ag := dbgen.ControllerAgent{Name: str("agname"), Command: str("agcmd"), Model: str("agmodel"), Auth: str("agauth"), AuthSecret: str("agsecret"), AuthEnv: str("agenv"), Enabled: 1}
		ag.MaxTurns, _ = strconv.ParseInt(str("agturns"), 10, 64)
		ag.TimeoutS, _ = strconv.ParseInt(str("agtimeout"), 10, 64)
		if err = h.D.Agents.Save(ctx, ag, flag("agdefault")); err == nil {
			msg = "saved " + ag.Name
		}
	case "agent-delete":
		if err = h.D.Agents.Delete(ctx, q.Get("name")); err == nil {
			msg = "removed " + q.Get("name")
		}
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
	case "instance-terminate":
		var name string
		name, err = api.TerminateOther(ctx, h.D, q.Get("region"), q.Get("iid"))
		msg = "terminating " + name
	case "instance-adopt":
		var res *api.AdoptResult
		res, err = api.Adopt(ctx, h.D, q.Get("region"), q.Get("iid"), str("adopttpl"))
		if err == nil {
			note := ""
			if !res.Reachable {
				note = `<p class="warn">` + template.HTMLEscapeString(res.Note) + `</p>`
			}
			mergeFragments(w, `<div id="adopt-result" class="adopt-result"><p>Run this on the instance (it needs the wrangl binary, from <code>`+template.HTMLEscapeString(res.Binary)+`</code>, and the token works once for 30 minutes):</p><pre>`+template.HTMLEscapeString(res.Command)+`</pre>`+note+`</div>`)
			msg = "token created"
		}
	case "task-queue":
		_, err = h.D.Plans.QueueWork(ctx, sqid)
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
			msg, nav = "deleted", "/tasks?project="+url.QueryEscape(p.Slug)
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
	case "project-limit":
		var n int64
		if n, err = strconv.ParseInt(q.Get("n"), 10, 64); err != nil || n < 0 {
			err = fmt.Errorf("limit must be a whole number, 0 or more")
			break
		}
		err = h.D.Projects.SetLimits(ctx, q.Get("slug"), map[string]int64{q.Get("template"): n})
		msg = fmt.Sprintf("%s limit on %s: %d", q.Get("slug"), q.Get("template"), n)
	case "repo-notes": // repo=owner/name gives it the notes role; empty repo clears
		err = h.D.Projects.SetNotesRepo(ctx, q.Get("slug"), q.Get("repo"))
		msg = "notes repo saved"
	case "repo-link":
		ref := str("repoadd")
		if ref == "" {
			ref = str("repoother")
		}
		_, err = h.D.Projects.LinkRepo(ctx, q.Get("slug"), ref, "")
		msg, reset = "repo linked", map[string]any{"repoadd": "", "repoother": ""}
		if err != nil && h.githubToast(ctx, w, err, "/project/"+q.Get("slug")) {
			return
		}
	case "template-save":
		var t dbgen.RunnerTemplate
		t, err = h.D.Fleet.Template(ctx, q.Get("name"))
		if err == nil {
			var body []byte
			if body, err = templateYAMLFromForm(t, str, flag); err == nil {
				var nt dbgen.RunnerTemplate
				if nt, err = h.D.Fleet.UpdateTemplate(ctx, t.Name, body); err == nil {
					msg = fmt.Sprintf("%s is now v%d", nt.Name, nt.Version)
					nav = "/settings"
				}
			}
		}
	case "account-add":
		_, err = h.D.Setup.AddAccount(ctx, str("acctlogin"), str("acctkind"))
		msg, reset = "account added", map[string]any{"acctlogin": ""}
		// No App yet means create it now: the setup page goes straight on to GitHub.
		nav = "/setup?account=" + url.QueryEscape(strings.TrimPrefix(str("acctlogin"), "@")) + "&create=1&back=" + url.QueryEscape(ghsetup.SafeBack(q.Get("back")))
	case "account-remove":
		err = h.D.Setup.RemoveAccount(ctx, q.Get("login"))
		msg = "account removed"
		nav = "/setup"
	case "repo-unlink":
		err = h.D.Projects.UnlinkRepo(ctx, q.Get("slug"), q.Get("repo"))
		msg = "repo unlinked"
	default:
		err = fmt.Errorf("unknown action")
	}
	if err != nil {
		// A missing or uninstalled GitHub App gets a link to the page that fixes it.
		back := ""
		switch r.PathValue("action") {
		case "pr", "answer":
			back = "/inbox"
		case "task-queue", "task-plan":
			back = "/tasks"
		}
		if h.githubToast(ctx, w, err, back) {
			return
		}
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

// logRow is one line of the Logs table: a run, or an operational entry (runner, template,
// upgrade). Runs are just one kind.
type logRow struct {
	At, Kind, Level, Subject, Title, Detail, Link, State string
}

// logRows merges runs and log entries, newest first. kind filters ("" = everything).
func (h *Handler) logRows(ctx context.Context, kind string) ([]logRow, error) {
	var out []logRow
	if kind == "" || kind == "run" {
		runs, err := h.D.Fleet.DB.RQ.ListRuns(ctx, dbgen.ListRunsParams{Lim: 200})
		if err != nil {
			return nil, err
		}
		for _, x := range runs {
			at := x.Run.QueuedAt
			if x.Run.FinishedAt.Valid {
				at = x.Run.FinishedAt.String
			} else if x.Run.StartedAt.Valid {
				at = x.Run.StartedAt.String
			}
			detail := x.ProjectSlug + "/" + x.TaskSqid
			if x.Run.Kind != "work" {
				detail += " · " + x.Run.Kind
			}
			level := "info"
			if x.Run.State == "failed" || x.Run.State == "needs_input" || x.Run.State == "blocked" {
				level = "warn"
			}
			out = append(out, logRow{At: at, Kind: "run", Level: level, Subject: x.RunnerName, State: x.Run.State,
				Title: fmt.Sprintf("#%d %s", x.Run.ID, x.TaskTitle), Detail: detail, Link: fmt.Sprintf("/run/%d", x.Run.ID)})
		}
	}
	if kind != "run" {
		ents, err := h.D.Fleet.DB.RQ.ListLogEntries(ctx, 300)
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			if kind != "" && e.Kind != kind {
				continue
			}
			l := ""
			if e.Kind == "runner" || e.Kind == "upgrade" || (e.Kind == "template" && !strings.HasPrefix(e.Title, "Template updated")) {
				l = "/runner/" + e.Subject
			}
			out = append(out, logRow{At: e.At, Kind: e.Kind, Level: e.Level, Subject: e.Subject, Title: e.Title, Detail: e.Detail, Link: l})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out, nil
}

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
	res, err := h.D.Setup.Exchange(r.Context(), q.Get("code"), q.Get("state"))
	if err != nil {
		http.Redirect(w, r, "/setup?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	to := "/setup?created=" + url.QueryEscape(res.Login)
	if res.Role != "" {
		to += "&role=" + url.QueryEscape(res.Role)
	}
	if res.Back != "" {
		to += "&back=" + url.QueryEscape(res.Back)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// acctView is a linked GitHub account on the Setup page.
type acctView struct {
	ghsetup.Account
	AppName     string
	State       string
	Manifest    string
	ManifestErr string
	Focus       bool
	Auto        bool // add-account just ran: submit the create-App form straight away
	JustCreated bool // the App was created a moment ago: send the owner on to install it
	// Status is what a live check of the App found: ok | noapp | noinstall | badcreds | error | unknown.
	Status, StatusText string
	NRepos             int
	// The reviewer App's own form (only filled while it does not exist yet).
	RName, RState, RManifest, RManifestErr string
}

// appHealth checks, with a short cache, that an account's App can really reach GitHub: it lists
// the repositories the App is installed on. "App ready" used to mean only that credentials were
// stored, which hid an installation that had gone away.
func (h *Handler) appHealth(ctx context.Context, ac ghsetup.Account) (status, text string, n int) {
	if ac.App == nil {
		return "noapp", "No App yet", 0
	}
	type cached struct {
		status, text string
		n            int
		at           time.Time
	}
	key := strings.ToLower(ac.Login)
	if v, ok := h.health.Load(key); ok {
		if c := v.(cached); time.Since(c.at) < 45*time.Second {
			return c.status, c.text, c.n
		}
	}
	if h.D.Projects == nil || h.D.Projects.GH == nil {
		return "unknown", "not checked", 0
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	repos, err := h.D.Projects.GH.ListRepos(cctx, ac.Login)
	var he *gh.HTTPError
	switch {
	case err == nil && len(repos) > 0:
		status, text, n = "ok", fmt.Sprintf("Ready · %d repositor%s", len(repos), map[bool]string{true: "y", false: "ies"}[len(repos) == 1]), len(repos)
	case err == nil:
		status, text = "noinstall", "Installed on no repositories"
	case errors.Is(err, gh.ErrNotInstalled) || (errors.As(err, &he) && he.Status == 404):
		status, text = "noinstall", "Not installed"
	case errors.As(err, &he) && (he.Status == 401 || he.Status == 403):
		status, text = "badcreds", "Credentials rejected"
	default:
		status, text = "error", "Could not check: "+shortErr(err)
	}
	h.health.Store(key, cached{status, text, n, time.Now()})
	return status, text, n
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}

// repoGroup is one account's repositories for the link-a-repo picker.
type repoGroup struct {
	Login, Kind string
	Repos       []gh.Repo
	NoApp       bool   // no App yet: offer to set it up
	Problem     string // the App exists but its repositories could not be listed
	InstallURL  string // where to add repositories to the App's installation
	SetupURL    string
}

// repoChoices lists, for each linked GitHub account, the repositories its App can see and that
// are not already linked to the project: what can be linked.
func (h *Handler) repoChoices(ctx context.Context, slug string, linked []dbgen.ListReposRow) []repoGroup {
	if h.D.Projects.GH == nil || h.D.Setup == nil {
		return nil
	}
	have := map[string]bool{}
	for _, l := range linked {
		have[strings.ToLower(l.Repo.Owner+"/"+l.Repo.Name)] = true
	}
	accts, _ := h.D.Setup.Accounts(ctx)
	var out []repoGroup
	for _, ac := range accts {
		g := repoGroup{Login: ac.Login, Kind: ac.Kind, SetupURL: "/setup?account=" + url.QueryEscape(ac.Login) + "&back=" + url.QueryEscape("/project/"+slug)}
		if ac.App == nil {
			g.NoApp = true
			out = append(out, g)
			continue
		}
		g.InstallURL = ac.App.InstallURL()
		repos, err := h.D.Projects.GH.ListRepos(ctx, ac.Login)
		var ni *gh.NotInstalledError
		switch {
		case errors.As(err, &ni):
			g.Problem = "the App is not installed on this account yet"
		case err != nil:
			g.Problem = err.Error()
		}
		for _, r := range repos {
			if !have[strings.ToLower(r.Owner+"/"+r.Name)] {
				g.Repos = append(g.Repos, r)
			}
		}
		out = append(out, g)
	}
	return out
}

// githubToast turns a GitHub App problem into a message with the next step as a link: create the
// App for that account, or install it on the repository. It reports whether err was one.
func (h *Handler) githubToast(ctx context.Context, w http.ResponseWriter, err error, back string) bool {
	var na *gh.NoAppError
	var ni *gh.NotInstalledError
	switch {
	case errors.As(err, &na):
		to := "/setup?account=" + url.QueryEscape(na.Owner)
		if na.Role != "reviewer" { // an owner we have never seen: add it and go straight to creating its App
			if _, ok := h.D.Setup.Account(ctx, na.Owner); !ok {
				h.D.Setup.AddAccount(ctx, na.Owner, "")
			}
			to += "&create=1"
		}
		if b := ghsetup.SafeBack(back); b != "" {
			to += "&back=" + url.QueryEscape(b)
		}
		label := "Set up the GitHub App for " + na.Owner
		if na.Role == "reviewer" {
			label = "Create the reviewer App for " + na.Owner
			to += "#reviewer-" + url.QueryEscape(na.Owner)
		}
		toastLink(w, err.Error(), to, label)
		goLater(w, to)
		return true
	case errors.As(err, &ni):
		label, to := "Open GitHub accounts", "/setup?account="+url.QueryEscape(ni.Owner)
		if ac, ok := h.D.Setup.Account(ctx, ni.Owner); ok {
			if ni.Role == "reviewer" && ac.Reviewer != nil {
				label, to = "Install the reviewer App on "+ni.Owner+"/"+ni.Name, ac.Reviewer.InstallURL()
			} else if ni.Role != "reviewer" && ac.App != nil {
				label, to = "Install the App on "+ni.Owner+"/"+ni.Name, ac.App.InstallURL()
			}
		}
		toastLink(w, err.Error(), to, label)
		goLater(w, to)
		return true
	}
	return false
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

// templateYAMLFromForm builds the template file for an edit: the editor's fields replace the
// stored ones, the agent block is kept as it is, and the usual validation (ParseTemplate, on
// save) has the last word.
func templateYAMLFromForm(t dbgen.RunnerTemplate, str func(string) string, _ func(string) bool) ([]byte, error) {
	var agent any
	if err := json.Unmarshal([]byte(t.AgentJson), &agent); err != nil {
		return nil, fmt.Errorf("stored agent: %w", err)
	}
	vcpu, err := strconv.Atoi(str("tpvcpu"))
	if err != nil {
		return nil, fmt.Errorf("min vCPU %q is not a number", str("tpvcpu"))
	}
	maxR, err := strconv.Atoi(str("tpmax"))
	if err != nil {
		return nil, fmt.Errorf("max runners %q is not a number", str("tpmax"))
	}
	maxS := int(t.MaxSessions) // a form without the field keeps the current value
	if v := str("tpsess"); v != "" {
		if maxS, err = strconv.Atoi(v); err != nil || maxS < 1 {
			return nil, fmt.Errorf("sessions per runner %q should be a number of at least 1", v)
		}
	}
	var types []string
	for _, x := range strings.Split(str("tptypes"), ",") {
		if x = strings.TrimSpace(x); x != "" {
			types = append(types, x)
		}
	}
	if types == nil {
		types = []string{}
	}
	doc := map[string]any{
		"name": t.Name, "description": str("tpdesc"), "arch": str("tparch"),
		"min":            map[string]any{"vcpu": vcpu, "ram": str("tpram"), "disk": str("tpdisk")},
		"instance_types": types, "max_runners": maxR, "max_sessions": maxS, "idle_timeout": str("tpidle"),
		"mise_toml": str("tpmise"), "setup_script": str("tpsetup"),
	}
	if str("tpimg") == "ami" {
		ami := str("tpami")
		if !strings.HasPrefix(ami, "ami-") {
			return nil, fmt.Errorf("AMI id %q should look like ami-0123abcd…", ami)
		}
		doc["ami"] = ami
	}
	if m, ok := agent.(map[string]any); ok && len(m) > 0 {
		doc["agent"] = agent
	}
	return yaml.Marshal(doc)
}

// shortDuration prints 30m, 2h or 90s instead of Go's 30m0s.
func shortDuration(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

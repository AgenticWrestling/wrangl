// Package web is the mobile-first PWA (spec §11): server-rendered html/template pages,
// made live with Datastar (vendored, served from the binary): every page opens one
// @get stream that morph-replaces its #main region whenever the event bus fires.
// Actions post Datastar signals to /ui/act/* and answer with a toast. Drag and drop
// (SortableJS) and the terminal (xterm.js over the PTY SSE stream) live in app.js.
package web

import (
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/dbtime"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/events"

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
	"hash/fnv"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/api"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/agents"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbtypes"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
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
	docs     sync.Map // run id -> *docEntry: the single Markdown file a run produced (Inbox)
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
		"dict":   func(k, v string) map[string]string { return map[string]string{k: v} },
		"ago":    func(ts string) string { return dbtime.Ago(ts, time.Now()) },
		"about":  firstLine,
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
		"sig":       func(prefix, name string) template.JS { return template.JS("$" + prefix + name) }, // a Datastar signal in an on:* expression (html/template would quote a string there)
		"lkey": func(r logRow) string { // stable per log row, so an expanded row stays open across live patches
			f := fnv.New32a()
			io.WriteString(f, r.Kind+"|"+r.At+"|"+r.Subject+"|"+r.Title)
			return fmt.Sprintf("lg%08x", f.Sum32())
		},
		"outline": outlineFromPayload,
		"ticon":   typeIcon,
		"termok":  termUsable,
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
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/projects", http.StatusFound) })
	for path, view := range map[string]string{
		"/projects": "projects", "/projects/{slug}": "tasks", "/projects/{slug}/settings": "project", "/task/{sqid}": "task", "/runners": "runners", "/runner/{name}": "runner",
		"/logs": "logs", "/run/{id}": "run", "/run/{id}/diff": "diff", "/run/{id}/files": "files",
		"/inbox": "inbox", "/settings": "settings", "/settings/templates/{name}": "template", "/setup": "setup",
	} {
		view := view
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) { h.page(w, r, view) })
	}
	// Old URLs keep working: /tasks (and its old name /backlog) is the project page, /project/{slug}
	// (goals & repos) is that project's settings page.
	toProject := func(frag string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			slug := q.Get("project")
			q.Del("project")
			to := "/projects"
			if slug != "" {
				to += "/" + url.PathEscape(slug)
			}
			if len(q) > 0 && slug != "" {
				to += "?" + q.Encode()
			}
			http.Redirect(w, r, to+frag, http.StatusFound)
		}
	}
	mux.HandleFunc("GET /tasks", toProject(""))
	mux.HandleFunc("GET /backlog", toProject("#backlog"))
	mux.HandleFunc("GET /project/{slug}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/projects/"+url.PathEscape(r.PathValue("slug"))+"/settings", http.StatusFound)
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
	mux.HandleFunc("GET /ui/transcript", h.transcriptMore)
	mux.HandleFunc("GET /ui/transcript-live", h.transcriptLive)
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
	if ps, err := h.D.Fleet.DB.RQ.ListProjects(r.Context(), "active"); err == nil { // left panel: the five most recently updated
		page["NavProjects"], _ = recentProjects(ps, 5)
	}
	page["Project"], _ = data["Project"].(string)
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

// attachDetail puts card on every node with the given sqid. A task can be listed more than once
// (a leaf is in the backlog tree and in the "In progress" leaf view, a run's root in its run
// entry), and only the one the page shows renders the card.
func attachDetail(nodes []*node, sqid string, card template.HTML) {
	for _, n := range nodes {
		if n.Sqid == sqid {
			n.Detail = card
		}
		attachDetail(n.Kids, sqid, card)
	}
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
	if _, ok := liveKinds[view]; !ok {
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
	push := func(wantMain, wantBadge bool) {
		var open []proto.QuestionView
		haveOpen := false
		if wantMain {
			data, err := h.data(r.Context(), view, r)
			if err != nil {
				return
			}
			open, haveOpen = data["OpenQuestions"].([]proto.QuestionView)
			if sq := sess.get(); view == "tasks" && sq != "" {
				card, err := h.taskPart(r.Context(), sq, "main-task")
				if err == nil {
					// The card goes wherever the page lists the task: the backlog tree, a run entry
					// (and its subtasks) in In progress / Queue, or the "In progress" leaf view.
					var all []*node
					if roots, ok := data["Roots"].([]*node); ok {
						all = append(all, roots...)
					}
					if now, ok := data["Now"].([]*node); ok {
						all = append(all, now...)
					}
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
			main, err := h.render("main-"+view, data)
			if err != nil {
				return
			}
			mergeFragments(w, `<main id="main">`+string(main)+`</main>`)
			// The sidebar's recent projects change with the tasks, so they ride along with #main.
			nav := map[string]any{}
			if ps, err := h.D.Fleet.DB.RQ.ListProjects(r.Context(), "active"); err == nil {
				nav["NavProjects"], _ = recentProjects(ps, 5)
			}
			nav["Project"], _ = data["Project"].(string)
			if html, err := h.render("subnav", nav); err == nil {
				mergeFragments(w, string(html))
			}
		}
		if wantBadge {
			if !haveOpen { // the inbox view already asked; every other page asks once, here
				open, _ = h.D.Runs.Questions(r.Context(), false)
			}
			mergeFragments(w, fmt.Sprintf(`<span id="inbox-badge" class="badge">%s</span>`, map[bool]string{true: strconv.Itoa(len(open)), false: ""}[len(open) > 0]))
		}
		if fl != nil {
			fl.Flush()
		}
	}
	push(true, true)
	keep := time.NewTicker(api.KeepAlive)
	defer keep.Stop()
	var debounce <-chan time.Time
	var wantMain, wantBadge bool
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sess.kick:
			push(true, false)
		case e := <-ch:
			// Only events that change this page re-render it; a question or PR change also moves the
			// Inbox badge, which every page shows.
			m, b := liveRelevant(view, e.Kind)
			if !m && !b {
				continue
			}
			wantMain, wantBadge = wantMain || m, wantBadge || b
			if debounce == nil {
				debounce = time.After(250 * time.Millisecond)
			}
		case <-debounce:
			debounce = nil
			push(wantMain, wantBadge)
			wantMain, wantBadge = false, false
		case <-keep.C:
			io.WriteString(w, ": ping\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

// liveKinds lists, per page, the bus event kinds that change what it shows. Anything else (a
// runner heartbeat on the Settings page, say) leaves it alone.
var liveKinds = map[string][]string{
	"tasks":    {events.KindTask, events.KindRun, events.KindPR, events.KindQuestion, events.KindThread},
	"projects": {events.KindTask, events.KindRun, events.KindPR, events.KindQuestion, events.KindRunner},
	"task":     {events.KindTask, events.KindRun, events.KindPR, events.KindQuestion, events.KindThread},
	"runners":  {events.KindRunner, events.KindRun},
	"runner":   {events.KindRunner, events.KindRun},
	"logs":     {events.KindLog, events.KindRun},
	"run":      {events.KindRun, events.KindLog, events.KindTask},
	"inbox":    {events.KindQuestion, events.KindPR, events.KindTask, events.KindRun, events.KindDoc},
	"settings": {events.KindTask, events.KindLog},
	"project":  {events.KindTask, events.KindPR, events.KindLog},
}

// liveRelevant says whether an event of this kind re-renders the page's #main, and whether it
// moves the Inbox badge.
func liveRelevant(view, kind string) (main, badge bool) {
	for _, k := range liveKinds[view] {
		if k == kind {
			main = true
		}
	}
	badge = kind == events.KindQuestion || kind == events.KindPR
	return
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
	Attention                bool          // an open question, or its latest run failed / is blocked
	AttnKind                 string        // "questions" or "failed" when Attention is set
	Kind                     string        // the one state shown by colour + glyph + label (see stateKind)
	Queueable                int           // open leaves in the subtree (or itself) that are neither queued nor running
	RollRev, RollQ, RollFail int           // descendants awaiting review / answers / failed (containers)
	Crumbs                   []crumb       // "Now" view: ancestors, root first
	Mockups                  int           // deliverables (mockups, prototypes, reports) handed back on this task
	Detail                   template.HTML // the expanded task's card, when this page has it open
	Change                   changeLink    // where GitHub shows what this task changed (checkpoint commit, else its PR)
	Kids                     []*node
}

// changeLink points at what changed on GitHub for a task: its checkpoint commit, or its PR.
type changeLink struct{ URL, Label, Title string }

// changeLinks maps task sqid to its change link, for every task of a project (projectID 0: all).
// A task's newest checkpoint commit wins; a task with none falls back to its newest PR.
func (h *Handler) changeLinks(ctx context.Context, projectID int64) map[string]changeLink {
	out := map[string]changeLink{}
	r := h.D.Fleet.DB.R
	rows, err := r.QueryContext(ctx, `SELECT t.sqid, c.sha, r.owner, r.name FROM task_checkpoint c
		JOIN task t ON t.id = c.task_id JOIN run ru ON ru.id = c.run_id JOIN repo r ON r.id = ru.repo_id
		WHERE c.id = (SELECT MAX(c2.id) FROM task_checkpoint c2 JOIN run r2 ON r2.id = c2.run_id WHERE c2.task_id = c.task_id AND (r2.pushed = 1 OR r2.state IN ('assigned', 'preparing', 'running', 'needs_input', 'pushing', 'cancelling'))) AND (?1 = 0 OR t.project_id = ?1)`, projectID)
	if err == nil {
		for rows.Next() {
			var sqid, sha, owner, repo string
			if rows.Scan(&sqid, &sha, &owner, &repo) == nil {
				short := sha
				if len(short) > 7 {
					short = short[:7]
				}
				out[sqid] = changeLink{"https://github.com/" + owner + "/" + repo + "/commit/" + sha, short, "What changed: commit " + short + " on GitHub"}
			}
		}
		rows.Close()
	}
	rows, err = r.QueryContext(ctx, `SELECT t.sqid, p.url, p.gh_number FROM pull_request p JOIN task t ON t.id = p.task_id
		WHERE p.id = (SELECT MAX(id) FROM pull_request WHERE task_id = p.task_id) AND (?1 = 0 OR t.project_id = ?1)`, projectID)
	if err == nil {
		for rows.Next() {
			var sqid, url string
			var n int64
			if rows.Scan(&sqid, &url, &n) == nil && url != "" {
				if _, ok := out[sqid]; !ok {
					out[sqid] = changeLink{url, fmt.Sprintf("PR #%d", n), "What changed: pull request on GitHub"}
				}
			}
		}
		rows.Close()
	}
	return out
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
		n := &node{TaskNode: proto.TaskNode{Sqid: r.Sqid, Title: r.Title, State: r.State, Type: string(r.TaskType), Depth: r.Depth, Children: r.Children}, Crumbs: up, Kind: "running"}
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
	Flat         bool            // In progress: Node is the leaf being worked, listed on its own
	ParentSqid   string          // Flat: the leaf's parent, for the jump button
	ParentTitle  string
}

// taskSections is what the Tasks page lists: running, queued and the rest.
type taskSections struct {
	Progress, Queue []*runEntry
	Backlog         []*node
	Now             []*node          // in-progress leaf tasks, with breadcrumbs
	Index           map[string]*node // every node of the project by sqid (incl. those shown as runs)
}

// stateKind collapses a task's state, latest-run trouble and lock into the single state shown
// to the owner. Failures outrank questions, which outrank review, then running, queued, done.
func stateKind(state, attn, lock string) string {
	switch {
	case state == "done" || state == "cancelled":
		return "done"
	case attn == "failed":
		return "failed"
	case attn == "questions":
		return "questions"
	case state == "in_review":
		return "review"
	case state == "in_progress" || (lock != "" && lock != "queued"):
		return "running"
	case lock == "queued":
		return "queued"
	}
	return "open"
}

// rollup fills the descendant counts of n and reports its subtree's totals including itself.
func rollup(n *node) (rev, q, fail int) {
	n.Queueable = 0
	if n.Children == 0 && n.State == "open" && (n.Kind == "open" || n.Kind == "failed" || n.Kind == "questions") && n.LockedBy == "" {
		n.Queueable = 1
	}
	for _, k := range n.Kids {
		r, qq, f := rollup(k)
		n.Queueable += k.Queueable
		n.RollRev, n.RollQ, n.RollFail = n.RollRev+r, n.RollQ+qq, n.RollFail+f
	}
	rev, q, fail = n.RollRev, n.RollQ, n.RollFail
	switch n.Kind {
	case "review":
		rev++
	case "questions":
		q++
	case "failed":
		fail++
	}
	return
}

// Awaits reports whether the task or a descendant waits for the owner.
func (n *node) Awaits() bool {
	return n.RollRev+n.RollQ+n.RollFail > 0 || n.Kind == "review" || n.Kind == "questions" || n.Kind == "failed"
}

type viewFn func(h *Handler, ctx context.Context, view string, r *http.Request, d map[string]any) error

// views maps a page name to the function that gathers its template data.
var views = map[string]viewFn{
	"tasks":    (*Handler).viewTasks,
	"projects": (*Handler).viewProjects,
	"task":     (*Handler).viewTask,
	"runners":  (*Handler).viewRunners,
	"runner":   (*Handler).viewRunner,
	"logs":     (*Handler).viewLogs,
	"run":      (*Handler).viewRun,
	"diff":     (*Handler).viewRun,
	"files":    (*Handler).viewRun,
	"inbox":    (*Handler).viewInbox,
	"project":  (*Handler).viewProject,
	"setup":    (*Handler).viewSetup,
	"template": (*Handler).viewTemplate,
	"settings": (*Handler).viewSettings,
}

func (h *Handler) data(ctx context.Context, view string, r *http.Request) (map[string]any, error) {
	d := map[string]any{}
	if fn := views[view]; fn != nil {
		if err := fn(h, ctx, view, r, d); err != nil {
			return nil, err
		}
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

// chatMsg is one message of a project chat, rendered for the panel.
type chatMsg struct {
	ID                      int64
	Role, Status, Agent, At string
	Body                    template.HTML
	Props                   []agents.Proposal
}

var codeSqidRE = regexp.MustCompile(`<code>([0-9A-Za-z]{3,16})</code>`)

// chatView loads the project's chat (task ids in agent replies become links to their rows), the
// context chip and whether any controller agent can answer.
func (h *Handler) chatView(ctx context.Context, slug string) ([]chatMsg, agents.ProjectContext, bool) {
	msgs, chip, err := h.D.Agents.ProjectThread(ctx, slug)
	if err != nil {
		return nil, chip, false
	}
	can := false
	if ags, _ := h.D.Agents.DB.RQ.ListControllerAgents(ctx); len(ags) > 0 {
		for _, a := range ags {
			can = can || a.Enabled == 1
		}
	}
	var out []chatMsg
	for _, m := range msgs {
		cm := chatMsg{ID: m.ID, Role: m.Role, Status: m.Status, Agent: m.Agent, At: m.At, Props: m.Proposals}
		if m.Role == "agent" {
			known := map[string]bool{}
			for _, t := range m.Tasks {
				known[t.Sqid] = true
			}
			html := string(markdown(m.Body))
			html = codeSqidRE.ReplaceAllStringFunc(html, func(s string) string {
				sq := codeSqidRE.FindStringSubmatch(s)[1]
				if !known[sq] {
					return s
				}
				return `<a class="tlink" href="/task/` + sq + `" data-goto="` + sq + `">` + s + `</a>`
			})
			cm.Body = template.HTML(html)
		} else {
			cm.Body = template.HTML("<p>" + strings.ReplaceAll(template.HTMLEscapeString(m.Body), "\n", "<br>") + "</p>")
		}
		out = append(out, cm)
	}
	return out, chip, can
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
	attn := h.attentionKinds(ctx, slug)
	changes := h.changeLinks(ctx, p.ID)
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
			Mockups: mockups[t.ID], Change: changes[t.Sqid]}
		if t.State != "done" && t.State != "cancelled" {
			n.AttnKind = attn[t.Sqid]
			n.Attention = n.AttnKind != ""
		}
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
		n.Kind = stateKind(t.State, n.AttnKind, lock)
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
	for _, n := range roots {
		rollup(n)
	}
	secs.Index = bySqid
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
			// In progress is a flat list of the leaves being worked (not the run's whole subtree);
			// a run with no current leaf yet is listed by its root task.
			if c := bySqid[r.CurrentSqid]; c != nil && len(c.Kids) == 0 {
				e.Node, e.Flat = c, true
			} else if len(n.Kids) == 0 {
				e.Flat = true
			}
			if e.Flat && e.Node.Parent != "" {
				if p := bySqid[e.Node.Parent]; p != nil {
					e.ParentSqid, e.ParentTitle = p.Sqid, p.Title
				}
			}
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
	for k := range h.attentionKinds(ctx, project) {
		out[k] = true
	}
	return out
}

// attentionKinds maps task sqid to "failed" (latest run failed or blocked) or "questions"
// (an open question, or the run waits for input). Failure wins when both hold.
func (h *Handler) attentionKinds(ctx context.Context, project string) map[string]string {
	out := map[string]string{}
	if qs, err := h.D.Runs.Questions(ctx, false); err == nil {
		for _, q := range qs {
			if q.Project == project {
				out[q.Task] = "questions"
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
		case "failed", "blocked":
			out[r.TaskSqid] = "failed"
		case "needs_input":
			out[r.TaskSqid] = "questions"
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
// actReq is one /ui/act/<action> request: the decoded Datastar signals and URL parameters.
type actReq struct {
	w    http.ResponseWriter
	r    *http.Request
	ctx  context.Context
	q    url.Values
	sig  map[string]any
	name string // the action
	sqid string
	id   int64
}

func (a *actReq) str(k string) string { v, _ := a.sig[k].(string); return strings.TrimSpace(v) }
func (a *actReq) flag(k string) bool  { v, _ := a.sig[k].(bool); return v }

// actResult is what an action reports on success (and, for back/dropEl, on failure too).
type actResult struct {
	msg    string         // the toast
	nav    string         // page to go to afterwards
	dropEl string         // id of a card to remove from the page
	flash  string         // sqid of the row that was acted on: it lights up after the live patch
	back   string         // where a missing/uninstalled GitHub App error should link to
	reset  map[string]any // signals to clear on success
	quiet  bool           // the action already patched the page: no toast
}

type actFn func(h *Handler, a *actReq) (actResult, error)

// needAgents refuses an action when the controller agents are not configured.
func needAgents(fn actFn) actFn {
	return func(h *Handler, a *actReq) (actResult, error) {
		if h.D.Agents == nil {
			return actResult{}, errs.Invalid
		}
		return fn(h, a)
	}
}

var actions = map[string]actFn{
	"task-new":               actTaskNew,
	"task-edit":              actTaskEdit,
	"task-ask":               needAgents(actTaskAsk),
	"proposal-apply":         needAgents(actProposalApply),
	"account-check":          actAccountCheck,
	"agent-save":             needAgents(actAgentSave),
	"agent-delete":           needAgents(actAgentDelete),
	"task-split":             actTaskSplit,
	"task-plan":              actTaskPlan,
	"instance-terminate":     actInstanceTerminate,
	"instance-adopt":         actInstanceAdopt,
	"task-queue":             actTaskQueue,
	"task-dequeue":           actTaskDequeue,
	"queue-move":             actQueueMove,
	"queue-all":              actQueueAll,
	"project-ask":            needAgents(actProjectAsk),
	"project-proposal-apply": needAgents(actProjectProposalApply),
	"project-archive":        actProjectArchive,
	"project-restore":        actProjectRestore,
	"task-complete":          actTaskState,
	"task-reopen":            actTaskState,
	"task-cancel":            actTaskState,
	"task-delete":            actTaskDelete,
	"task-needs":             actTaskNeeds,
	"answer":                 actAnswer,
	"pr":                     actPR,
	"pr-resolve":             actPRResolve,
	"run-cancel":             actRunCancel,
	"runner":                 actRunner,
	"fleet-launch":           actFleetLaunch,
	"project-new":            actProjectNew,
	"project-save":           actProjectSave,
	"project-limit":          actProjectLimit,
	"repo-notes":             actRepoNotes,
	"repo-link":              actRepoLink,
	"template-save":          actTemplateSave,
	"tpl-ami-check":          actTplAmiCheck,
	"tpl-ami-test":           actTplAmiTest,
	"tpl-market":             actTplMarket,
	"account-add":            actAccountAdd,
	"account-remove":         actAccountRemove,
	"repo-unlink":            actRepoUnlink,
}

func (h *Handler) act(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a := &actReq{w: w, r: r, ctx: ctx, q: r.URL.Query(), name: r.PathValue("action")}
	json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&a.sig)
	a.sqid = a.q.Get("sqid")
	a.id, _ = strconv.ParseInt(a.q.Get("id"), 10, 64)
	fn := actions[a.name]
	if fn == nil {
		toast(w, "unknown action", true)
		return
	}
	res, err := fn(h, a)
	if err != nil {
		// A missing or uninstalled GitHub App gets a link to the page that fixes it.
		if h.githubToast(ctx, w, err, res.back) {
			return
		}
		if res.dropEl != "" && errors.Is(err, errs.Conflict) { // already handled elsewhere (second click, another tab): the card is stale
			executeScript(w, fmt.Sprintf("document.getElementById(%q)?.remove()", res.dropEl))
		}
		toast(w, err.Error(), true)
		return
	}
	if !res.quiet {
		toast(w, res.msg, false)
	}
	if res.reset != nil {
		mergeSignals(w, res.reset)
	}
	if res.dropEl != "" {
		executeScript(w, fmt.Sprintf("document.getElementById(%q)?.remove()", res.dropEl))
	}
	if res.flash != "" { // the row that was acted on lights up once the live patch has re-rendered it
		executeScript(w, fmt.Sprintf("window.wrFlash&&wrFlash(%q)", res.flash))
	}
	if res.nav != "" {
		executeScript(w, fmt.Sprintf("window.location.assign(%q)", res.nav))
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
		g := repoGroup{Login: ac.Login, Kind: ac.Kind, SetupURL: "/setup?account=" + url.QueryEscape(ac.Login) + "&back=" + url.QueryEscape("/projects/"+slug+"/settings")}
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
func templateYAMLFromForm(t dbgen.RunnerTemplate, str func(string) string) ([]byte, error) {
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

// Package tui is the full-screen dashboard (`wrangl tui`): runners, runs, the work stack,
// questions and PRs, refreshed live from the controller's event stream.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/client"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// API is the slice of the controller client the TUI uses (faked in tests).
type API interface {
	Runners(ctx context.Context, all bool) ([]proto.RunnerView, error)
	Runs(ctx context.Context, state, runner, project string, limit int) ([]proto.RunView, error)
	Projects(ctx context.Context) ([]proto.ProjectView, error)
	ProjectTree(ctx context.Context, project string, all bool) ([]proto.TaskNode, error)
	Questions(ctx context.Context, all bool) ([]proto.QuestionView, error)
	PRs(ctx context.Context, state string) ([]proto.PRView, error)
	CreateTask(ctx context.Context, req proto.TaskCreateRequest) (proto.TaskRef, error)
	MoveTask(ctx context.Context, sqid string, req proto.TaskMoveRequest) (proto.TaskRef, error)
	NeedsAdd(ctx context.Context, sqid, pre string) error
	QueueTask(ctx context.Context, sqid string) (int64, string, error)
	TaskAction(ctx context.Context, sqid, op, message string) error
	SplitTask(ctx context.Context, sqid string, req proto.TaskSplitRequest) ([]proto.TaskRef, error)
	Answer(ctx context.Context, id int64, value, text string) error
	PRAction(ctx context.Context, id int64, op, message string) error
	CancelRun(ctx context.Context, id int64) error
	RunnerAction(ctx context.Context, name, action string) error
	Terminate(ctx context.Context, name string) error
	Events(ctx context.Context, fn func(client.Event)) error
}

// Tabs.
const (
	tabRunners = iota
	tabRuns
	tabStack
	tabQuestions
	tabPRs
	numTabs
)

var tabNames = []string{"Runners", "Runs", "Stack", "Questions", "PRs"}

type data struct {
	runners   []proto.RunnerView
	runs      []proto.RunView
	projects  []proto.ProjectView
	tree      []proto.TaskNode
	questions []proto.QuestionView
	prs       []proto.PRView
}

type (
	dataMsg   struct{ d data }
	errMsg    struct{ err error }
	statusMsg string
	eventMsg  struct{}
	reloadMsg struct{ gen int }
)

type modal struct {
	prompt   string
	value    string
	confirm  bool // y/n rather than text
	onSubmit func(string) tea.Cmd
}

// Model is the bubbletea model.
type Model struct {
	api     API
	ctx     context.Context
	exec    func(args ...string) tea.Cmd // runs a wrangl subcommand in the terminal
	tab     int
	cursor  [numTabs]int
	project int
	d       data
	status  string
	modal   *modal
	events  chan struct{}
	gen     int
	width   int
}

// New builds the model. exec runs `wrangl <args>` with the terminal handed over.
func New(ctx context.Context, api API, exec func(args ...string) tea.Cmd) Model {
	return Model{api: api, ctx: ctx, exec: exec, events: make(chan struct{}, 1), status: "loading…"}
}

// Init loads data and starts listening for change notices.
func (m Model) Init() tea.Cmd {
	go func() {
		for m.ctx.Err() == nil {
			m.api.Events(m.ctx, func(client.Event) {
				select {
				case m.events <- struct{}{}:
				default:
				}
			})
			select { // reconnect after a break
			case <-m.ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}()
	return tea.Batch(m.load(), m.waitEvent(), tick())
}

func tick() tea.Cmd { return tea.Tick(10*time.Second, func(time.Time) tea.Msg { return eventMsg{} }) }

func (m Model) waitEvent() tea.Cmd {
	return func() tea.Msg {
		select {
		case <-m.events:
			return eventMsg{}
		case <-m.ctx.Done():
			return nil
		}
	}
}

func (m Model) projectSlug() string {
	if len(m.d.projects) == 0 {
		return ""
	}
	return m.d.projects[m.project%len(m.d.projects)].Slug
}

func (m Model) load() tea.Cmd {
	ctx, api, proj := m.ctx, m.api, m.projectSlug()
	return func() tea.Msg {
		var d data
		var err error
		if d.runners, err = api.Runners(ctx, false); err != nil {
			return errMsg{err}
		}
		if d.runs, err = api.Runs(ctx, "", "", "", 50); err != nil {
			return errMsg{err}
		}
		if d.projects, err = api.Projects(ctx); err != nil {
			return errMsg{err}
		}
		if proj == "" && len(d.projects) > 0 {
			proj = d.projects[0].Slug
		}
		if proj != "" {
			if d.tree, err = api.ProjectTree(ctx, proj, false); err != nil {
				return errMsg{err}
			}
		}
		if d.questions, err = api.Questions(ctx, false); err != nil {
			return errMsg{err}
		}
		if d.prs, err = api.PRs(ctx, "open"); err != nil {
			return errMsg{err}
		}
		return dataMsg{d}
	}
}

// do runs an action, then reports and reloads.
func (m Model) do(desc string, f func() error) tea.Cmd {
	return func() tea.Msg {
		if err := f(); err != nil {
			return errMsg{err}
		}
		return statusMsg(desc)
	}
}

func (m Model) rows() int {
	switch m.tab {
	case tabRunners:
		return len(m.d.runners)
	case tabRuns:
		return len(m.d.runs)
	case tabStack:
		return len(m.d.tree)
	case tabQuestions:
		return len(m.d.questions)
	}
	return len(m.d.prs)
}

func (m *Model) clamp() {
	for t := 0; t < numTabs; t++ {
		n := map[int]int{tabRunners: len(m.d.runners), tabRuns: len(m.d.runs), tabStack: len(m.d.tree), tabQuestions: len(m.d.questions), tabPRs: len(m.d.prs)}[t]
		if m.cursor[t] >= n {
			m.cursor[t] = max(0, n-1)
		}
	}
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case dataMsg:
		m.d = msg.d
		m.clamp()
		if strings.HasPrefix(m.status, "loading") {
			m.status = ""
		}
	case errMsg:
		m.status = "error: " + msg.err.Error()
	case statusMsg:
		m.status = string(msg)
		return m, m.load()
	case eventMsg:
		m.gen++ // debounce bursts of notices into one reload
		gen := m.gen
		return m, tea.Batch(tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return reloadMsg{gen} }), m.waitEvent())
	case reloadMsg:
		if msg.gen == m.gen {
			return m, tea.Batch(m.load(), tick())
		}
	case tea.KeyMsg:
		if m.modal != nil {
			return m.modalKey(msg)
		}
		return m.key(msg)
	}
	return m, nil
}

func (m Model) modalKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	md := m.modal
	if md.confirm {
		m.modal = nil
		if k.String() == "y" {
			return m, md.onSubmit("y")
		}
		m.status = "cancelled"
		return m, nil
	}
	switch k.Type {
	case tea.KeyEsc:
		m.modal, m.status = nil, "cancelled"
	case tea.KeyEnter:
		m.modal = nil
		if strings.TrimSpace(md.value) == "" {
			return m, nil
		}
		return m, md.onSubmit(md.value)
	case tea.KeyBackspace:
		if r := []rune(md.value); len(r) > 0 {
			md.value = string(r[:len(r)-1])
		}
	case tea.KeySpace:
		md.value += " "
	case tea.KeyRunes:
		md.value += string(k.Runes)
	}
	return m, nil
}

func (m *Model) ask(prompt string, f func(string) tea.Cmd) {
	m.modal = &modal{prompt: prompt, onSubmit: f}
}

func (m *Model) confirm(prompt string, f func() tea.Cmd) {
	m.modal = &modal{prompt: prompt + " [y/N]", confirm: true, onSubmit: func(string) tea.Cmd { return f() }}
}

// siblings returns the indexes of the tree rows sharing node i's parent, in order.
func (m Model) siblings(i int) []int {
	var out []int
	for j, n := range m.d.tree {
		if n.Parent == m.d.tree[i].Parent && n.Depth == m.d.tree[i].Depth {
			out = append(out, j)
		}
	}
	return out
}

func (m Model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	ctx, api := m.ctx, m.api
	s := k.String()
	switch s {
	case "ctrl+c", "esc":
		return m, tea.Quit
	case "tab":
		m.tab = (m.tab + 1) % numTabs
		return m, nil
	case "shift+tab":
		m.tab = (m.tab + numTabs - 1) % numTabs
		return m, nil
	case "j", "down":
		if m.cursor[m.tab] < m.rows()-1 {
			m.cursor[m.tab]++
		}
		return m, nil
	case "k", "up":
		if m.cursor[m.tab] > 0 {
			m.cursor[m.tab]--
		}
		return m, nil
	case "r":
		m.status = "reloading…"
		return m, m.load()
	}
	if m.tab != tabQuestions && len(s) == 1 && s[0] >= '1' && s[0] <= '5' {
		m.tab = int(s[0] - '1')
		return m, nil
	}
	cur := m.cursor[m.tab]
	if m.rows() == 0 && !(m.tab == tabStack && (s == "n" || s == "p")) {
		return m, nil
	}
	switch m.tab {
	case tabRunners:
		r := m.d.runners[cur]
		switch s {
		case "c":
			act := "cordon"
			if r.Cordoned {
				act = "uncordon"
			}
			return m, m.do(r.Name+": "+act, func() error { return api.RunnerAction(ctx, r.Name, act) })
		case "D":
			m.confirm("drain "+r.Name+"?", func() tea.Cmd {
				return m.do(r.Name+": drain", func() error { return api.RunnerAction(ctx, r.Name, "drain") })
			})
		case "T":
			m.confirm("terminate "+r.Name+" now?", func() tea.Cmd {
				return m.do(r.Name+": terminated", func() error { return api.Terminate(ctx, r.Name) })
			})
		}
	case tabRuns:
		r := m.d.runs[cur]
		id := fmt.Sprint(r.ID)
		switch s {
		case "a":
			return m, m.exec("run", "attach", id)
		case "l":
			return m, m.exec("logs", id, "-f")
		case "d":
			return m, m.exec("run", "diff", id)
		case "enter":
			return m, m.exec("run", "show", id)
		case "x":
			m.confirm(fmt.Sprintf("cancel run %d?", r.ID), func() tea.Cmd {
				return m.do("cancelling run "+id, func() error { return api.CancelRun(ctx, r.ID) })
			})
		}
	case tabStack:
		proj := m.projectSlug()
		var n proto.TaskNode
		if len(m.d.tree) > 0 {
			n = m.d.tree[cur]
		}
		switch s {
		case "p":
			if len(m.d.projects) > 0 {
				m.project = (m.project + 1) % len(m.d.projects)
				m.cursor[tabStack] = 0
				m.status = "project " + m.projectSlug()
				return m, m.load()
			}
		case "n", "o":
			parent, after, what := n.Sqid, "", "new subtask of "+n.Sqid
			if s == "o" || len(m.d.tree) == 0 {
				parent, after, what = n.Parent, n.Sqid, "new task after "+n.Sqid
			}
			if len(m.d.tree) == 0 {
				parent, after, what = "", "", "new top-level task in "+proj
			}
			m.ask(what+": title", func(title string) tea.Cmd {
				return m.do("created "+title, func() error {
					_, err := api.CreateTask(ctx, proto.TaskCreateRequest{Project: proj, Parent: parent, After: after, Title: title})
					return err
				})
			})
		case "J", "K":
			sib := m.siblings(cur)
			for i, j := range sib {
				if j != cur {
					continue
				}
				var req proto.TaskMoveRequest
				switch {
				case s == "J" && i+1 < len(sib):
					req.After = m.d.tree[sib[i+1]].Sqid
				case s == "K" && i > 0:
					req.Before = m.d.tree[sib[i-1]].Sqid
				default:
					return m, nil
				}
				return m, m.do("moved "+n.Sqid, func() error { _, err := api.MoveTask(ctx, n.Sqid, req); return err })
			}
		case ">":
			sib := m.siblings(cur)
			for i, j := range sib {
				if j == cur && i > 0 {
					p := m.d.tree[sib[i-1]].Sqid
					return m, m.do("indented "+n.Sqid, func() error { _, err := api.MoveTask(ctx, n.Sqid, proto.TaskMoveRequest{Parent: &p}); return err })
				}
			}
		case "<":
			if n.Parent == "" {
				return m, nil
			}
			var grand string
			for _, x := range m.d.tree {
				if x.Sqid == n.Parent {
					grand = x.Parent
				}
			}
			req := proto.TaskMoveRequest{After: n.Parent, Root: grand == ""}
			if grand != "" {
				req.Parent = &grand
			}
			return m, m.do("outdented "+n.Sqid, func() error { _, err := api.MoveTask(ctx, n.Sqid, req); return err })
		case "N":
			m.ask(n.Sqid+" needs (sqid)", func(pre string) tea.Cmd {
				return m.do(n.Sqid+" needs "+pre, func() error { return api.NeedsAdd(ctx, n.Sqid, strings.TrimSpace(pre)) })
			})
		case "Q":
			return m, m.do("queued "+n.Sqid, func() error { _, _, err := api.QueueTask(ctx, n.Sqid); return err })
		case "x":
			return m, m.do("completed "+n.Sqid, func() error { return api.TaskAction(ctx, n.Sqid, "complete", "") })
		case "c":
			m.confirm("cancel "+n.Sqid+" and its unfinished subtasks?", func() tea.Cmd {
				return m.do("cancelled "+n.Sqid, func() error { return api.TaskAction(ctx, n.Sqid, "cancel", "") })
			})
		case "s":
			m.ask("split "+n.Sqid+" into (titles separated by ;)", func(v string) tea.Cmd {
				var kids []proto.SplitNode
				for _, t := range strings.Split(v, ";") {
					if t = strings.TrimSpace(t); t != "" {
						kids = append(kids, proto.SplitNode{Title: t})
					}
				}
				return m.do(fmt.Sprintf("split %s into %d", n.Sqid, len(kids)), func() error {
					_, err := api.SplitTask(ctx, n.Sqid, proto.TaskSplitRequest{Children: kids})
					return err
				})
			})
		case "enter":
			return m, m.exec("task", "show", n.Sqid)
		}
	case tabQuestions:
		q := m.d.questions[cur]
		switch {
		case len(s) == 1 && s[0] >= '1' && s[0] <= '9':
			i := int(s[0] - '1')
			if i < len(q.Options) {
				o := q.Options[i]
				if o.Value == "request_changes" {
					m.ask("what should change?", func(text string) tea.Cmd {
						return m.do("requested changes", func() error { return api.Answer(ctx, q.ID, o.Value, text) })
					})
					return m, nil
				}
				return m, m.do("answered: "+o.Label, func() error { return api.Answer(ctx, q.ID, o.Value, "") })
			}
		case s == "t" && q.AllowFreeText:
			m.ask("answer", func(text string) tea.Cmd {
				return m.do("answered", func() error { return api.Answer(ctx, q.ID, "", text) })
			})
		case s == "enter":
			return m, m.exec("question", "show", fmt.Sprint(q.ID))
		}
	case tabPRs:
		p := m.d.prs[cur]
		switch s {
		case "A":
			m.confirm(fmt.Sprintf("approve and merge #%d?", p.Number), func() tea.Cmd {
				return m.do(fmt.Sprintf("merged #%d", p.Number), func() error { return api.PRAction(ctx, p.ID, "approve", "") })
			})
		case "R":
			m.ask("what should change?", func(text string) tea.Cmd {
				return m.do("requested changes", func() error { return api.PRAction(ctx, p.ID, "request-changes", text) })
			})
		case "C":
			m.confirm(fmt.Sprintf("close #%d without merging?", p.Number), func() tea.Cmd {
				return m.do(fmt.Sprintf("closed #%d", p.Number), func() error { return api.PRAction(ctx, p.ID, "close", "") })
			})
		case "enter":
			return m, m.exec("pr", "show", fmt.Sprint(p.ID))
		}
	}
	return m, nil
}

var (
	stTab    = lipgloss.NewStyle().Padding(0, 1)
	stActive = stTab.Bold(true).Reverse(true)
	stSel    = lipgloss.NewStyle().Reverse(true)
	stDim    = lipgloss.NewStyle().Faint(true)
	stWarn   = lipgloss.NewStyle().Bold(true)
)

var glyph = map[string]string{"open": "○", "in_progress": "▶", "in_review": "◐", "done": "✓", "cancelled": "✗"}

func pct(m map[string]any, k string) string {
	if v, ok := m[k].(float64); ok {
		return fmt.Sprintf("%3.0f%%", v)
	}
	return "  - "
}

// View renders the screen.
func (m Model) View() string {
	var b strings.Builder
	counts := []int{len(m.d.runners), len(m.d.runs), len(m.d.tree), len(m.d.questions), len(m.d.prs)}
	for i, name := range tabNames {
		label := fmt.Sprintf("%d %s (%d)", i+1, name, counts[i])
		if i == tabQuestions && counts[i] > 0 {
			label = fmt.Sprintf("%d %s (%d!)", i+1, name, counts[i])
		}
		if i == m.tab {
			b.WriteString(stActive.Render(label))
		} else {
			b.WriteString(stTab.Render(label))
		}
	}
	b.WriteString("\n\n")
	var lines []string
	switch m.tab {
	case tabRunners:
		for _, r := range m.d.runners {
			var mt map[string]any
			_ = jsonUnmarshal(r.Metrics, &mt)
			state := r.State
			if r.Cordoned {
				state += " (cordoned)"
			}
			lines = append(lines, fmt.Sprintf("%-22s %-20s %-12s cpu %s mem %s disk %s  v%d/%d %s",
				r.Name, state, r.Template, pct(mt, "cpu_percent"), pct(mt, "mem_percent"), pct(mt, "disk_percent"), r.TemplateVersion, r.WantVersion, r.InstanceType))
		}
	case tabRuns:
		for _, r := range m.d.runs {
			lines = append(lines, fmt.Sprintf("%-5d %-12s %-8s %-7s %-14s %s %s  %s", r.ID, r.State, r.Kind, r.Project, r.Runner, r.Task, r.TaskTitle, r.Status))
		}
	case tabStack:
		b.WriteString(stDim.Render("project: "+m.projectSlug()+"  (p: next project)") + "\n")
		for _, n := range m.d.tree {
			extra := ""
			if n.Unmet > 0 {
				extra += fmt.Sprintf("  ⏸ waits on %d", n.Unmet)
			}
			if n.LockedBy != "" {
				extra += "  🔒 " + n.LockedBy
			}
			title := n.Title
			if m := typeMark[n.Type]; m != "" {
				title = m + " " + title
			}
			lines = append(lines, fmt.Sprintf("%s%s %s %s%s", strings.Repeat("  ", int(n.Depth)), n.Sqid, glyph[n.State], title, extra))
		}
	case tabQuestions:
		for _, q := range m.d.questions {
			var opts []string
			for i, o := range q.Options {
				opts = append(opts, fmt.Sprintf("%d) %s", i+1, o.Label))
			}
			if q.AllowFreeText {
				opts = append(opts, "t) text")
			}
			lines = append(lines, fmt.Sprintf("#%-4d %-9s %s %s — %s   [%s]", q.ID, q.Kind, q.Task, q.TaskTitle, q.Prompt, strings.Join(opts, "  ")))
		}
	case tabPRs:
		for _, p := range m.d.prs {
			lines = append(lines, fmt.Sprintf("#%-4d %-18s checks %-8s risk %-6s %s %s", p.Number, p.Repo, dash(p.Checks), dash(p.Risk), p.Task, p.Title))
		}
	}
	if len(lines) == 0 {
		b.WriteString(stDim.Render("  (nothing here)") + "\n")
	}
	for i, l := range lines {
		if i == m.cursor[m.tab] {
			b.WriteString(stSel.Render(l))
		} else {
			b.WriteString(l)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	if m.modal != nil {
		b.WriteString(stWarn.Render(m.modal.prompt+": ") + m.modal.value)
		if !m.modal.confirm {
			b.WriteString("▏  " + stDim.Render("(enter to submit, esc to cancel)"))
		}
		b.WriteString("\n")
	} else {
		b.WriteString(stDim.Render(help[m.tab]) + "\n")
	}
	if m.status != "" {
		b.WriteString(m.status + "\n")
	}
	return b.String()
}

var help = map[int]string{
	tabRunners:   "j/k move · c cordon/uncordon · D drain · T terminate · tab/1-5 panes · r reload · esc quit",
	tabRuns:      "j/k move · a attach · l logs · d diff · enter show · x cancel · tab panes · esc quit",
	tabStack:     "n new subtask · o new sibling · J/K rerank · >/< indent/outdent · N needs · s split · Q queue · x complete · c cancel · p project · enter show",
	tabQuestions: "1-9 choose option · t free text · enter details · tab panes · esc quit",
	tabPRs:       "A approve+merge · R request changes · C close · enter show · tab panes · esc quit",
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// typeMark marks non-code task types in the work-stack pane.
var typeMark = map[string]string{"ux": "[ux]", "architecture": "[arch]", "spec": "[docs]", "docs": "[docs]", "bug": "[bug]", "research": "[research]", "review": "[review]", "release": "[RELEASE]", "milestone": "[MILESTONE]", "feature": "[FEATURE]"}

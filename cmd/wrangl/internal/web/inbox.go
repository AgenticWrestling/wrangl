package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"net/url"
	"strings"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// Every Inbox item (a question, a task review, a plan proposal, a pull request) is one inboxItem
// rendered by the single "icard" template: the same header, body, optional comment box and
// action row. A kind only adds what is needed to resolve it.

type inboxAction struct {
	Label, URL, Class, Confirm, Busy, Tip string
	NeedsText                             bool // opens the comment box first when it is empty
}

type inboxLink struct {
	Text, URL string
	External  bool
}

type inboxPill struct{ Text, Class string }

type inboxDetail struct {
	Summary string
	HTML    template.HTML
	Pre     string
	Open    bool
}

type inboxItem struct {
	DOM, Key, Bind  string // element id, signal key (b<Key>, c<Key>), bound text signal
	Kind            string // question kind, or pr_review
	Tag, TagClass   string
	Task, TaskTitle string
	RunID           int64
	Links           []inboxLink
	Pills           []inboxPill
	Age             string
	Title           string
	Body            template.HTML
	Details         []inboxDetail
	Chat            bool
	ChatHint        string
	Actions         []inboxAction
	Changes         changeLink
}

// inboxItems builds the Inbox: questions of every kind, then open pull requests.
func (h *Handler) inboxItems(ctx context.Context, qs []proto.QuestionView, prs []dbgen.ListPRsRow, changes map[string]changeLink) []inboxItem {
	var out []inboxItem
	for _, q := range qs {
		it := inboxItem{
			DOM: fmt.Sprintf("q%d", q.ID), Key: fmt.Sprintf("q%d", q.ID), Bind: fmt.Sprintf("ans%d", q.ID),
			Kind: q.Kind, Tag: "? needs input", TagClass: "k-questions", Task: q.Task, TaskTitle: q.TaskTitle, RunID: q.RunID,
			Age: ago(q.CreatedAt), Title: q.Prompt, Chat: q.AllowFreeText, ChatHint: "Reply / feedback", Changes: changes[q.Task],
		}
		switch q.Kind {
		case "plan_review":
			it.Tag = "◐ plan review"
			it.Details = append(it.Details, inboxDetail{Summary: "Proposed split", Pre: outlineFromPayload(q.Payload), Open: true})
		case "task_review":
			it.Tag = "◐ task review"
		}
		if q.DetailMD != "" {
			it.Body = markdown(q.DetailMD)
		}
		if q.Kind == "task_review" && q.RunID != 0 && taskReviewReason(q.Payload) == "review" {
			if doc := h.runDoc(ctx, q.RunID); doc != nil {
				it.Details = append(it.Details, inboxDetail{Summary: doc.Name, HTML: doc.HTML, Open: true})
			}
		}
		id := q.ID
		for _, o := range q.Options {
			cls := ""
			if o.Value == "accept" || o.Value == "approve" || o.Value == "yes" {
				cls = "primary"
			}
			it.Actions = append(it.Actions, inboxAction{
				Label: o.Label, Tip: o.Description, Class: cls, NeedsText: o.Value == "reopen",
				URL: fmt.Sprintf("/ui/act/answer?id=%d&value=%s", id, url.QueryEscape(o.Value)),
			})
		}
		if q.AllowFreeText && len(q.Options) == 0 {
			it.Actions = append(it.Actions, inboxAction{Label: "Send", Class: "primary", URL: fmt.Sprintf("/ui/act/answer?id=%d", id)})
		}
		out = append(out, it)
	}
	for _, r := range prs {
		p := r.PullRequest
		it := inboxItem{
			DOM: fmt.Sprintf("pr-%d", p.ID), Key: fmt.Sprintf("pr%d", p.ID), Bind: fmt.Sprintf("pr%d", p.ID),
			Kind: "pr_review", Tag: "◐ in review", TagClass: "k-review", Task: r.TaskSqid, TaskTitle: r.TaskTitle, RunID: p.RunID,
			Age: ago(p.CreatedAt), Title: fmt.Sprintf("Review pull request %s#%d", r.Repo, p.GhNumber),
			Chat: true, ChatHint: "What should change? (for Request changes)", Changes: changes[r.TaskSqid],
		}
		it.Links = append(it.Links,
			inboxLink{Text: fmt.Sprintf("%s#%d", r.Repo, p.GhNumber), URL: p.Url, External: true},
			inboxLink{Text: "diff", URL: fmt.Sprintf("/run/%d/diff", p.RunID)})
		it.Pills = append(it.Pills, inboxPill{Text: p.State})
		if p.Risk.Valid {
			it.Pills = append(it.Pills, inboxPill{Text: "risk: " + p.Risk.String, Class: "risk-" + p.Risk.String})
		}
		conflicted := p.Mergeable == "conflicted"
		if conflicted {
			it.Pills = append(it.Pills, inboxPill{Text: "conflicts", Class: "risk-high"})
		}
		if p.Checks != "" {
			it.Pills = append(it.Pills, inboxPill{Text: "checks: " + p.Checks})
		}
		if p.ReviewSummaryMd != "" {
			it.Details = append(it.Details, inboxDetail{Summary: "Review", HTML: markdown(p.ReviewSummaryMd)})
		}
		it.Actions = nil
		if conflicted {
			it.Actions = append(it.Actions, inboxAction{Label: "Smart Merge", Class: "primary", Confirm: "Queue an agent run that merges the base branch into the PR branch and resolves the conflicts?", Busy: "Queuing…", URL: fmt.Sprintf("/ui/act/pr-resolve?id=%d", p.ID)})
		}
		it.Actions = append(it.Actions, []inboxAction{
			{Label: "Approve & merge", Class: map[bool]string{false: "primary"}[!conflicted], Confirm: "Approve and merge?", Busy: "Merging…", URL: fmt.Sprintf("/ui/act/pr?id=%d&op=approve", p.ID)},
			{Label: "Request changes", NeedsText: true, URL: fmt.Sprintf("/ui/act/pr?id=%d&op=request-changes", p.ID)},
			{Label: "Close", Class: "danger", Confirm: "Close without merging?", URL: fmt.Sprintf("/ui/act/pr?id=%d&op=close", p.ID)},
		}...)
		out = append(out, it)
	}
	return out
}

func taskReviewReason(payload json.RawMessage) string {
	var p struct {
		Reason string `json:"reason"`
	}
	json.Unmarshal(payload, &p)
	return p.Reason
}

// runDoc is the single Markdown file a run produced (a spec, a design, a report), rendered, or
// nil when the run produced anything else. Reading it asks the run's runner, which can take a
// while, so it is fetched in the background and cached; the page gets it on the next live patch.
type runDocT struct {
	Name string
	HTML template.HTML
}

type docEntry struct {
	doc     *runDocT
	pending bool
	at      time.Time
}

func (h *Handler) runDoc(ctx context.Context, runID int64) *runDocT {
	if v, ok := h.docs.Load(runID); ok {
		e := v.(*docEntry)
		if e.pending || e.doc != nil || time.Since(e.at) < 5*time.Minute {
			return e.doc
		}
	}
	h.docs.Store(runID, &docEntry{pending: true, at: time.Now()})
	go func() {
		fctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		doc := h.fetchRunDoc(fctx, runID)
		h.docs.Store(runID, &docEntry{doc: doc, at: time.Now()})
		if doc != nil {
			h.D.Bus.Publish("question", "doc")
		}
	}()
	return nil
}

func (h *Handler) fetchRunDoc(ctx context.Context, runID int64) *runDocT {
	db := h.D.Fleet.DB
	if h.D.Deliverables != nil {
		rows, _ := db.RQ.ListDeliverablesForRun(ctx, sql.NullInt64{Int64: runID, Valid: true})
		if len(rows) == 1 && rows[0].Kind == "report_md" {
			if v := h.deliverableViews(ctx, rows); len(v) == 1 && v[0].Report != "" {
				return &runDocT{Name: rows[0].Title, HTML: v[0].Report}
			}
		}
	}
	out, err := h.D.Runs.Inspect(ctx, runID, "diff", "", false)
	if err != nil {
		return nil
	}
	files := parseDiff(out.Data)
	if len(files) != 1 || !strings.HasSuffix(strings.ToLower(files[0].Name), ".md") {
		return nil
	}
	f, err := h.D.Runs.Inspect(ctx, runID, "file", files[0].Name, false)
	if err != nil || strings.TrimSpace(f.Data) == "" {
		return nil
	}
	return &runDocT{Name: files[0].Name, HTML: markdown(f.Data)}
}

// Package deliverables stores what a run hands back besides a pull request: mockup images,
// a prototype link (for example a Claude Artifact), a link, or a markdown report. Files live
// under the data dir; anything older than Retention is swept.
package deliverables

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nreid/devhost/cmd/wrangl/internal/controller/runs"
	"github.com/nreid/devhost/cmd/wrangl/internal/db"
	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbgen"
	"github.com/nreid/devhost/cmd/wrangl/internal/errs"
	"github.com/nreid/devhost/cmd/wrangl/internal/events"
	"github.com/nreid/devhost/cmd/wrangl/internal/proto"
	"github.com/nreid/devhost/cmd/wrangl/internal/stack"
)

const (
	// Retention is how long deliverables are kept.
	Retention = 90 * 24 * time.Hour
	// MaxImage and MaxReport bound what one call may store.
	MaxImage  = 5 << 20
	MaxReport = 1 << 20
)

// Service owns the deliverable table and its files.
type Service struct {
	DB  *db.DB
	Dir string // <data dir>/deliverables
	Bus *events.Bus
	Log *slog.Logger
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// sniffImage returns the file extension and MIME type of PNG, JPEG, WebP or GIF data.
func sniffImage(b []byte) (ext, mime string, ok bool) {
	switch {
	case len(b) > 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "png", "image/png", true
	case len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "jpg", "image/jpeg", true
	case len(b) > 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "webp", "image/webp", true
	case len(b) > 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "gif", "image/gif", true
	}
	return "", "", false
}

func invalid(f string, a ...any) error {
	return fmt.Errorf("%w: "+f, append([]any{errs.Invalid}, a...)...)
}

// Submit implements the submit_deliverable tool for work and revise runs.
func (s *Service) Submit(ctx context.Context, sc runs.Scope, raw json.RawMessage) (any, error) {
	var a struct {
		Kind        string `json:"kind"`
		Title       string `json:"title"`
		URL         string `json:"url"`
		ContentMD   string `json:"content_md"`
		ImageBase64 string `json:"image_base64"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, invalid("%v", err)
	}
	if sc.RunID == 0 {
		return nil, invalid("submit_deliverable is for agent runs")
	}
	run, err := s.DB.RQ.GetRun(ctx, sc.RunID)
	if err != nil {
		return nil, err
	}
	taskID := run.TaskID
	if run.CurrentTaskID.Valid {
		taskID = run.CurrentTaskID.Int64
	}
	title := strings.TrimSpace(a.Title)
	if len(title) > 200 {
		title = title[:200]
	}

	p := dbgen.InsertDeliverableParams{TaskID: taskID, RunID: sql.NullInt64{Int64: sc.RunID, Valid: true}, Kind: a.Kind, Title: title}
	var data []byte
	var ext string
	switch a.Kind {
	case "artifact_url", "link":
		u, err := url.Parse(strings.TrimSpace(a.URL))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, invalid("url must be an http(s) URL")
		}
		p.Url = u.String()
	case "image":
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(a.ImageBase64))
		if err != nil {
			return nil, invalid("image_base64: %v", err)
		}
		if len(b) > MaxImage {
			return nil, invalid("image is %d bytes; the limit is %d", len(b), MaxImage)
		}
		var ok bool
		if ext, p.Mime, ok = sniffImage(b); !ok {
			return nil, invalid("image_base64 must be a PNG, JPEG, WebP or GIF")
		}
		data = b
	case "report_md":
		if strings.TrimSpace(a.ContentMD) == "" {
			return nil, invalid("content_md is required for a report")
		}
		if len(a.ContentMD) > MaxReport {
			return nil, invalid("report is %d bytes; the limit is %d", len(a.ContentMD), MaxReport)
		}
		data, ext, p.Mime = []byte(a.ContentMD), "md", "text/markdown; charset=utf-8"
	default:
		return nil, invalid("kind must be image, artifact_url, link or report_md")
	}

	if data != nil {
		if err := os.MkdirAll(s.Dir, 0o750); err != nil {
			return nil, err
		}
		p.File = fmt.Sprintf("%d-%d.%s", sc.RunID, s.now().UnixNano(), ext)
		if err := os.WriteFile(filepath.Join(s.Dir, p.File), data, 0o640); err != nil {
			return nil, err
		}
	}
	d, err := s.DB.Q.InsertDeliverable(ctx, p)
	if err != nil {
		if p.File != "" {
			os.Remove(filepath.Join(s.Dir, p.File))
		}
		return nil, err
	}
	if s.Bus != nil {
		s.Bus.Publish("task", fmt.Sprint(taskID))
	}
	return map[string]any{"ok": true, "id": d.ID, "note": "Recorded. The owner will see it on the task."}, nil
}

// Open returns a deliverable's row and the path of its file ("" for URL kinds).
func (s *Service) Open(ctx context.Context, id int64) (dbgen.Deliverable, string, error) {
	d, err := s.DB.RQ.GetDeliverable(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return d, "", fmt.Errorf("%w: deliverable %d", errs.NotFound, id)
	} else if err != nil {
		return d, "", err
	}
	if d.File == "" {
		return d, "", nil
	}
	// The name was generated here; refuse anything that is not a plain file name.
	if d.File != filepath.Base(d.File) || strings.HasPrefix(d.File, ".") {
		return d, "", fmt.Errorf("%w: bad file name", errs.Invalid)
	}
	return d, filepath.Join(s.Dir, d.File), nil
}

// Sweep deletes deliverables (rows and files) created before now-retention. It returns how
// many were removed.
func (s *Service) Sweep(ctx context.Context, retention time.Duration) (int, error) {
	cutoff := s.now().Add(-retention).Format("2006-01-02T15:04:05.000Z")
	n := 0
	for {
		rows, err := s.DB.RQ.ExpiredDeliverables(ctx, cutoff)
		if err != nil {
			return n, err
		}
		if len(rows) == 0 {
			return n, nil
		}
		for _, d := range rows {
			if d.File != "" && d.File == filepath.Base(d.File) {
				if err := os.Remove(filepath.Join(s.Dir, d.File)); err != nil && !errors.Is(err, os.ErrNotExist) {
					s.Log.Warn("remove deliverable file", "id", d.ID, "err", err)
				}
			}
			if err := s.DB.Q.DeleteDeliverable(ctx, d.ID); err != nil {
				return n, err
			}
			n++
		}
	}
}

// Loop sweeps once a day until ctx ends.
func (s *Service) Loop(ctx context.Context) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		if n, err := s.Sweep(ctx, Retention); err != nil {
			s.Log.Error("deliverable sweep", "err", err)
		} else if n > 0 {
			s.Log.Info("deliverables expired", "removed", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// --- acceptance without a pull request ------------------------------------------------------

// Review is what the deliverable-review flow needs from the rest of the controller.
type Review struct {
	Runs  *runs.Service
	Stack *stack.Service
}

func nint(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

// OnRunFinished raises a deliverable_review question when a work or revise run completed
// tasks but pushed nothing and handed back deliverables: there is no PR for the owner to
// review, so the owner reviews the deliverables themselves.
func (s *Service) OnRunFinished(ctx context.Context, rv Review, runID int64) {
	if err := s.onRunFinished(ctx, rv, runID); err != nil {
		s.Log.Error("deliverable review after run", "run", runID, "err", err)
	}
}

func (s *Service) onRunFinished(ctx context.Context, rv Review, runID int64) error {
	run, err := s.DB.RQ.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if (run.Kind != "work" && run.Kind != "revise") || run.Pushed == 1 {
		return nil
	}
	nd, err := s.DB.RQ.CountDeliverablesForRun(ctx, nint(runID))
	if err != nil || nd == 0 {
		return err
	}
	nt, err := s.DB.RQ.CountRunReviewTasks(ctx, nint(runID))
	if err != nil || nt == 0 {
		return err
	}
	if n, err := s.DB.RQ.OpenDeliverableQuestion(ctx, runID); err != nil || n > 0 {
		return err
	}
	root, err := s.DB.RQ.GetTask(ctx, run.TaskID)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]int64{"run_id": runID})
	_, err = rv.Runs.NewQuestion(ctx, dbgen.CreateQuestionParams{TaskID: root.ID, Kind: "deliverable_review",
		Prompt:        fmt.Sprintf("Review the deliverables for %s %q", root.Sqid.String, root.Title),
		DetailMd:      fmt.Sprintf("%d deliverable(s) on the task page: /task/%s#deliverables", nd, root.Sqid.String),
		AllowFreeText: 1, PayloadJson: string(payload)}, []proto.QuestionOption{
		{Label: "Approve", Value: "approve", Description: "mark the work done"},
		{Label: "Request changes", Value: "request_changes", Description: "say what to change in the text"},
	})
	return err
}

// Answer handles an answered deliverable_review question.
func (s *Service) Answer(rv Review) runs.AnswerHandler {
	return func(ctx context.Context, q dbgen.Question, value, text string) error {
		var p struct {
			RunID int64 `json:"run_id"`
		}
		json.Unmarshal([]byte(q.PayloadJson), &p)
		done, _ := s.DB.RQ.CompletedInRun(ctx, nint(p.RunID))
		switch value {
		case "approve":
			if err := s.DB.Q.MarkRunTasksDone(ctx, nint(p.RunID)); err != nil {
				return err
			}
			for _, d := range done {
				rv.Stack.Recompute(ctx, d.Sqid)
			}
			s.Log.Info("deliverables approved", "run", p.RunID)
			return nil
		case "request_changes":
			text = strings.TrimSpace(text)
			if text == "" {
				return invalid("say what to change")
			}
			if err := s.DB.Q.ReopenRunTasks(ctx, nint(p.RunID)); err != nil {
				return err
			}
			// The feedback goes into the task's prompt so the next run (and the history) has it.
			for _, d := range done {
				t, err := rv.Stack.Task(ctx, s.DB.RQ, d.Sqid)
				if err != nil {
					continue
				}
				prompt := strings.TrimRight(t.Prompt, "\n") + "\n\n---\n**Owner feedback on the previous deliverables:**\n\n" + text + "\n"
				if _, err := rv.Stack.Edit(ctx, d.Sqid, nil, &prompt, stack.User); err != nil {
					return err
				}
			}
			root, err := s.DB.RQ.GetTask(ctx, q.TaskID)
			if err != nil {
				return err
			}
			if _, err := rv.Stack.Queue(ctx, root.Sqid.String); err != nil {
				return fmt.Errorf("queue the revision: %w", err)
			}
			return nil
		}
		return nil
	}
}

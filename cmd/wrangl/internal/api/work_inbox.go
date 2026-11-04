package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// pathID reads a positive integer path parameter; what names it in the error.
func pathID(r *http.Request, name, what string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: %s %q", errs.Invalid, what, r.PathValue(name))
	}
	return id, nil
}

func prView(x dbgen.ListPRsRow) proto.PRView {
	v := proto.PRView{ID: x.PullRequest.ID, Number: x.PullRequest.GhNumber, URL: x.PullRequest.Url, Repo: x.Repo, Task: x.TaskSqid,
		Title: x.TaskTitle, Branch: x.PullRequest.HeadBranch, State: x.PullRequest.State, Risk: x.PullRequest.Risk.String,
		Decision: x.PullRequest.Decision.String, Checks: x.PullRequest.Checks, Review: x.PullRequest.ReviewSummaryMd}
	if g := x.PullRequest.GatesJson; g != "" && g != "{}" {
		v.Gates = json.RawMessage(g)
	}
	return v
}

// registerQuestions serves the Inbox: agent questions and pull requests.
func registerQuestions(mux *http.ServeMux, d Deps) {
	db := d.Fleet.DB
	mux.HandleFunc("GET /api/v1/questions", getJSON(func(r *http.Request) ([]proto.QuestionView, error) {
		return d.Runs.Questions(r.Context(), r.URL.Query().Get("all") == "1")
	}))
	mux.HandleFunc("GET /api/v1/questions/{id}", getJSON(func(r *http.Request) (proto.QuestionView, error) {
		id, err := pathID(r, "id", "question id")
		if err != nil {
			return proto.QuestionView{}, err
		}
		return d.Runs.Question(r.Context(), id)
	}))
	mux.HandleFunc("POST /api/v1/questions/{id}/answer", func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r, "id", "question id") // checked before the body, as the other id routes do
		if err != nil {
			writeErr(w, err)
			return
		}
		handle(http.StatusNoContent, func(r *http.Request, req proto.AnswerRequest) (none, error) {
			return none{}, d.Runs.Answer(r.Context(), id, req.Value, req.Text)
		})(w, r)
	})

	// --- pull requests ---
	mux.HandleFunc("GET /api/v1/prs", getJSON(func(r *http.Request) ([]proto.PRView, error) {
		state := r.URL.Query().Get("state")
		if state == "" {
			state = "open"
		}
		rows, err := db.RQ.ListPRs(r.Context(), state)
		if err != nil {
			return nil, err
		}
		out := []proto.PRView{}
		for _, x := range rows {
			out = append(out, prView(x))
		}
		return out, nil
	}))
	mux.HandleFunc("GET /api/v1/prs/{id}", getJSON(func(r *http.Request) (proto.PRView, error) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		rows, err := db.RQ.ListPRs(r.Context(), "all")
		if err != nil {
			return proto.PRView{}, err
		}
		for _, x := range rows {
			if x.PullRequest.ID == id {
				return prView(x), nil
			}
		}
		return proto.PRView{}, fmt.Errorf("%w: PR %d", errs.NotFound, id)
	}))
	for _, op := range []string{"approve", "request-changes", "resolve", "close"} {
		mux.HandleFunc("POST /api/v1/prs/{id}/"+op, func(w http.ResponseWriter, r *http.Request) {
			id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
			if err != nil {
				writeErr(w, fmt.Errorf("%w: PR id", errs.Invalid))
				return
			}
			handleOpt(http.StatusNoContent, func(r *http.Request, req proto.MessageRequest) (none, error) {
				switch op {
				case "approve":
					err = d.PRs.Approve(r.Context(), id)
				case "request-changes":
					err = d.PRs.RequestChanges(r.Context(), id, req.Message)
				case "resolve":
					err = d.PRs.ResolveConflicts(r.Context(), id)
				case "close":
					err = d.PRs.Close(r.Context(), id)
				}
				return none{}, err
			})(w, r)
		})
	}
}

package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

func noRows(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", errs.NotFound, what)
	}
	return err
}

// registerWork serves the work stack API: projects, tasks, the Inbox and runs, one file each.
func registerWork(mux *http.ServeMux, d Deps) {
	registerProjects(mux, d)
	registerTasks(mux, d)
	registerQuestions(mux, d)
	registerRuns(mux, d)
}

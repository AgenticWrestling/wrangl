package api

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/errs"
)

// SecretStore is the controller's secret files, as the API sees them. Values never leave it.
type SecretStore struct {
	Set  func(name string, r io.Reader) error
	List func() ([]string, error)
	Has  func(name string) bool
	Get  func(name string) ([]byte, error) // for controller-side executors only; never served
}

func registerSecrets(mux *http.ServeMux, d Deps) {
	if d.Secrets == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		names, err := d.Secrets.List()
		if err != nil {
			writeErr(w, err)
			return
		}
		if names == nil {
			names = []string{}
		}
		writeJSON(w, http.StatusOK, names) // names only, never values
	})
	mux.HandleFunc("PUT /api/v1/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10+1))
		if err != nil {
			writeErr(w, err)
			return
		}
		if len(body) > 64<<10 {
			writeErr(w, fmt.Errorf("%w: secret is larger than 64 KiB", errs.Invalid))
			return
		}
		if strings.TrimSpace(string(body)) == "" {
			writeErr(w, fmt.Errorf("%w: secret is empty", errs.Invalid))
			return
		}
		if err := d.Secrets.Set(r.PathValue("name"), strings.NewReader(string(body))); err != nil {
			writeErr(w, fmt.Errorf("%w: %v", errs.Invalid, err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

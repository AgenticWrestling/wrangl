package api

import "net/http"

// none stands for "no request body" or "no response body" in handle.
type none struct{}

// handle adapts a typed function to an http.HandlerFunc: it decodes the JSON body into Req
// (skipped when Req is none), calls fn, and writes Resp as JSON with the given status. A Resp of
// none writes just the status (204 for ack). Errors go through writeErr.
func handle[Req, Resp any](status int, fn func(r *http.Request, req Req) (Resp, error)) http.HandlerFunc {
	return adapt(status, true, fn)
}

// handleOpt is handle for requests whose body may be omitted entirely.
func handleOpt[Req, Resp any](status int, fn func(r *http.Request, req Req) (Resp, error)) http.HandlerFunc {
	return adapt(status, false, fn)
}

func adapt[Req, Resp any](status int, bodyRequired bool, fn func(r *http.Request, req Req) (Resp, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req Req
		if _, noBody := any(req).(none); !noBody && (bodyRequired || r.ContentLength > 0) {
			if err := decode(r, &req); err != nil {
				writeErr(w, err)
				return
			}
		}
		resp, err := fn(r, req)
		if err != nil {
			writeErr(w, err)
			return
		}
		if _, empty := any(resp).(none); empty {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, status, resp)
	}
}

// getJSON is handle for requests without a body that answer 200 with JSON.
func getJSON[Resp any](fn func(r *http.Request) (Resp, error)) http.HandlerFunc {
	return handle(http.StatusOK, func(r *http.Request, _ none) (Resp, error) { return fn(r) })
}

// ack is handle for requests without a body that answer with just a status (204 by default).
func ack(status int, fn func(r *http.Request) error) http.HandlerFunc {
	return handle(status, func(r *http.Request, _ none) (none, error) { return none{}, fn(r) })
}

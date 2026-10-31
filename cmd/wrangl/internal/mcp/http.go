package mcp

import (
	"io"
	"net/http"
)

// HTTPHandler serves MCP's Streamable HTTP transport in its simplest form: each POST
// carries one JSON-RPC message or batch and gets a JSON response (no server-initiated
// streams). GET returns 405, as the spec allows.
func (s *Server) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out := s.Handle(r.Context(), body)
		if out == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	})
}

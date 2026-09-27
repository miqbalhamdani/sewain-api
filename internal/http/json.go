package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// writeJSON is the success counterpart to writeError in problem.go.
//
// The status is written before the body, so a marshalling failure here cannot
// change it -- by then the client already has 200 and the only honest thing
// left is a log line carrying the trace id.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.ErrorContext(r.Context(), "write response body",
			"error", err, "path", r.URL.Path)
	}
}

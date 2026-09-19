package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// writeError writes any error as an RFC 9457 application/problem+json response.
//
// Handlers pass whatever they have. Anything that is not a known error becomes
// an internal one, whose detail is fixed -- a database message or a file path
// reaching a response body is a disclosure, and the useful version of it goes
// to the log next to the trace id instead.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	problem := apperrors.From(err)
	traceID := apperrors.TraceID(r.Context())

	// Logged before the write, and with the same id the caller is about to be
	// given. That pairing is what makes the id worth quoting: the client gets a
	// string, the operator gets the cause.
	level := slog.LevelWarn
	if problem.Status >= http.StatusInternalServerError {
		level = slog.LevelError
	}
	if traceID == "" {
		// No recording span, so the id a client is about to be handed resolves
		// to nothing. That is a missing telemetry.Setup, and it is silent from
		// the outside -- the response still looks well formed.
		slog.ErrorContext(r.Context(),
			"no trace id for an error response; telemetry was never started")
	}
	slog.LogAttrs(r.Context(), level, "request failed",
		slog.String("code", problem.Code),
		slog.Int("status", problem.Status),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("trace_id", traceID),
		slog.String("cause", causeOf(problem)),
	)

	body := Problem{
		Type:     "https://sewain.id/problems/" + problem.Code,
		Title:    problem.Title,
		Status:   problem.Status,
		Detail:   &problem.Detail,
		Instance: ptr(r.URL.Path),
		TraceId:  traceID,
	}
	if len(problem.Fields) > 0 {
		fields := make([]ProblemError, 0, len(problem.Fields))
		for _, f := range problem.Fields {
			pe := ProblemError{Field: f.Name}
			if f.Detail != "" {
				pe.Detail = ptr(f.Detail)
			}
			for k, v := range f.Extra {
				pe.Set(k, v)
			}
			fields = append(fields, pe)
		}
		body.Errors = &fields
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(problem.Status)
	_ = json.NewEncoder(w).Encode(body)
}

func causeOf(err *apperrors.Error) string {
	if unwrapped := err.Unwrap(); unwrapped != nil {
		return unwrapped.Error()
	}
	return err.Detail
}

func ptr[T any](v T) *T { return &v }

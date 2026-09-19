package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestErrorsCarryAResolvableTraceID is S1-011's acceptance: every error carries
// a trace_id resolvable to a span.
//
// "Resolvable" is the load-bearing word, so the test does not merely assert the
// field is non-empty -- an all-zero id, a random UUID or a fresh id per response
// would all pass that. It records the spans the server actually produced and
// requires the id in the body to be one of them.
func TestErrorsCarryAResolvableTraceID(t *testing.T) {
	spans := recordSpans(t)

	srv := newServer(t)

	// One request per way a request can fail, so no error path is left able to
	// answer without an id. None of the four reaches the database -- they fail
	// in the middleware or in request decoding -- so this test seeds nothing.

	for _, tt := range []struct {
		name       string
		request    func(t *testing.T) *http.Request
		wantStatus int
		wantCode   string
	}{
		{
			name:       "401 from the authentication middleware",
			request:    func(t *testing.T) *http.Request { return httptest.NewRequest(http.MethodGet, "/api/v1/me", nil) },
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthenticated",
		},
		{
			name: "401 from an invalid token",
			request: func(t *testing.T) *http.Request {
				return bearerRequest(t, http.MethodGet, "/api/v1/me", "not.a.token")
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthenticated",
		},
		// A 403 case belongs here and is deliberately absent: every route in
		// this phase is reachable by both roles, so there is nothing to be
		// denied. It returns with S1-009, the first owner-only endpoint.
		//
		// Adding a fake 403 here would be testing the fake.
		{
			name: "422 from the request body",
			request: func(t *testing.T) *http.Request {
				return jsonRequest(t, "/api/v1/auth/login", map[string]string{"email": "", "password": ""})
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "validation-failed",
		},
		{
			name: "401 from a handler",
			request: func(t *testing.T) *http.Request {
				return jsonRequest(t, "/api/v1/auth/login", map[string]string{
					"email": "nobody@example.com", "password": "not the password",
				})
			},
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthenticated",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(spans.Ended())

			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, tt.request(t))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d\nbody: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("Content-Type = %q, want application/problem+json", ct)
			}

			var problem struct {
				Type    string `json:"type"`
				Title   string `json:"title"`
				Status  int    `json:"status"`
				TraceID string `json:"trace_id"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&problem); err != nil {
				t.Fatalf("decode problem: %v", err)
			}

			if problem.TraceID == "" {
				t.Fatal("no trace_id")
			}
			// 32 hex characters, per the example in API spec.md 1.1. All zeroes
			// is what the default no-op tracer produces, and it resolves to
			// nothing -- the exact failure this test exists to catch.
			if len(problem.TraceID) != 32 {
				t.Errorf("trace_id %q is %d characters, want 32", problem.TraceID, len(problem.TraceID))
			}
			if problem.TraceID == "00000000000000000000000000000000" {
				t.Error("trace_id is all zeroes; no SDK tracer provider is installed")
			}

			// The acceptance. The id must belong to a span this request
			// actually produced.
			ended := spans.Ended()
			if len(ended) <= before {
				t.Fatalf("the request recorded no span, so nothing could resolve the id")
			}
			var found bool
			for _, span := range ended[before:] {
				if span.SpanContext().TraceID().String() == problem.TraceID {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("trace_id %s matches no span this request produced", problem.TraceID)
			}

			if !strings.HasSuffix(problem.Type, tt.wantCode) {
				t.Errorf("type = %q, want it to end in %q", problem.Type, tt.wantCode)
			}
			if problem.Status != rec.Code {
				t.Errorf("body says %d, response says %d", problem.Status, rec.Code)
			}
			if problem.Title == "" {
				t.Error("no title")
			}
		})
	}

	// Two requests are two traces. A single id reused across requests would
	// satisfy every check above and be useless in a support ticket.
	t.Run("each request gets its own trace", func(t *testing.T) {
		first := traceIDOf(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
		second := traceIDOf(t, srv, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))

		if first == second {
			t.Errorf("both requests reported trace_id %s", first)
		}
	})

	// An internal failure must not describe itself. The detail is fixed so a
	// database message or a file path cannot reach a response body; the real
	// cause goes to the log beside the same id.
	t.Run("an internal error says nothing about itself", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, jsonRequest(t, "/api/v1/auth/login", map[string]string{
			"email": "nobody@example.com", "password": "not the password",
		}))

		body := rec.Body.String()
		for _, leak := range []string{"pgx", "postgres", "SQLSTATE", "app_user", "/Users/"} {
			if strings.Contains(body, leak) {
				t.Errorf("response mentions %q: %s", leak, body)
			}
		}
	})
}

// recordSpans installs an SDK tracer provider that keeps every span in memory,
// and restores whatever was there afterwards.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	recorder := tracetest.NewSpanRecorder()
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
	return recorder
}

func traceIDOf(t *testing.T, srv http.Handler, r *http.Request) string {
	t.Helper()

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)

	var problem struct {
		TraceID string `json:"trace_id"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	return problem.TraceID
}

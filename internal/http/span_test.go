package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestSpansCarryOwnerAndQueryDuration is the half of S1-011's acceptance that
// the trace id tests do not reach.
//
// BR-092 asks for three things on the span: owner_id, route, and query
// duration. The trace id being resolvable was already proven; these are not.
func TestSpansCarryOwnerAndQueryDuration(t *testing.T) {
	ctx := t.Context()
	store := openAppStore(ctx, t)
	spans := recordSpans(t)
	srv := newServer(t)

	ownerID := uuid.Must(uuid.NewV7())
	s := seedSignedInUser(ctx, t, store, ownerID)

	before := len(spans.Ended())

	// GET /me is the smallest authenticated route that touches the database,
	// so one request exercises all three attributes.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, bearerRequest(t, http.MethodGet, "/api/v1/me", s.accessToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\nbody: %s", rec.Code, rec.Body)
	}

	ended := spans.Ended()[before:]
	if len(ended) == 0 {
		t.Fatal("the request recorded no spans at all")
	}

	var server, query sdktrace.ReadOnlySpan
	for _, span := range ended {
		switch {
		case span.Name() == "postgres.query":
			query = span
		case attrOf(span, "owner_id") != "":
			server = span
		}
	}

	t.Run("owner_id is on the server span", func(t *testing.T) {
		if server == nil {
			t.Fatalf("no span carries owner_id; got %s", spanNames(ended))
		}
		if got := attrOf(server, "owner_id"); got != ownerID.String() {
			t.Errorf("owner_id = %q, want %q", got, ownerID.String())
		}
	})

	// The route is the span name rather than an attribute, and it is the
	// pattern rather than the URL -- otherwise every booking id would be its
	// own operation in the tracing backend.
	t.Run("the route names the span", func(t *testing.T) {
		if server == nil {
			t.Skip("no server span")
		}
		if want := "GET /api/v1/me"; server.Name() != want {
			t.Errorf("span name = %q, want %q", server.Name(), want)
		}
	})

	// A span is a duration, so a child span per query is the duration without
	// anything having to measure it.
	t.Run("each query has its own span", func(t *testing.T) {
		if query == nil {
			t.Fatalf("no postgres.query span; got %s", spanNames(ended))
		}
		if query.EndTime().Sub(query.StartTime()) <= 0 {
			t.Error("the query span has no duration")
		}
		if got := attrOf(query, "db.query.text"); got == "" {
			t.Error("the query span does not carry the SQL it ran")
		}
	})

	// Arguments carry emails, password hashes and identity numbers. A tracing
	// backend is a place where anyone with access would read them.
	t.Run("no query span carries arguments", func(t *testing.T) {
		for _, span := range ended {
			if span.Name() != "postgres.query" {
				continue
			}
			for _, kv := range span.Attributes() {
				if string(kv.Key) == "db.query.parameter" ||
					string(kv.Key) == "db.query.args" {
					t.Errorf("query span carries %s", kv.Key)
				}
			}
		}
	})
}

func attrOf(span sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsString()
		}
	}
	return ""
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, 0, len(spans))
	for _, s := range spans {
		names = append(names, s.Name())
	}
	return names
}

package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// BasePath is the prefix every contract endpoint lives under, and the only
// place it is written down. cmd/api mounts on it and NewRouter registers on it,
// so the two cannot drift -- they did once, and the result was a binary that
// served nothing but /healthz while every test stayed green, because the tests
// build the handler directly and never exercise the mount.
//
// It matches the `servers:` entries in ../docs/openapi.yaml.
const BasePath = "/api/v1"

// NewRouter mounts the generated routes under BasePath behind authentication.
//
// The registration itself comes from openapi.yaml via oapi-codegen -- no route
// in this file, and none hand-written anywhere else. An endpoint exists because
// the contract says so.
func NewRouter(srv ServerInterface, signer *auth.Signer, store IdempotencyStore) http.Handler {
	r := chi.NewRouter()
	// Outermost, so a span exists before anything can fail. An error raised by
	// the authentication middleware still carries a trace id that resolves.
	// Order matters. A span before anything can fail, then who you are, then
	// whether you have proved your address, then whether this exact intent has
	// been seen before.
	//
	// The verification gate runs after Authenticate because an anonymous
	// request should hear "no token", not "not verified". Idempotency runs
	// last of the four because its key is scoped per owner, and there is no
	// owner until Authenticate has run.
	r.Use(tracing, Authenticate(signer), RequireVerifiedEmail(signer), Idempotency(store))
	return HandlerWithOptions(srv, ChiServerOptions{
		BaseURL:    BasePath,
		BaseRouter: r,
		// The generated default is http.Error -- plain text, no trace id, and
		// a 400 whatever went wrong. Everything else in this service answers
		// RFC 9457, and a malformed uuid in a path is not the one place where
		// that should stop being true (BR-092).
		ErrorHandlerFunc: badRequest,
	})
}

// badRequest turns the generated parameter-binding failures into the same
// problem envelope every handler uses.
func badRequest(w http.ResponseWriter, r *http.Request, err error) {
	writeError(w, r, apperrors.ValidationFailed(
		"A path or query parameter is not in the expected format.").WithCause(err))
}

// tracing starts a span per request and names it after the matched route
// pattern rather than the URL, so /api/v1/products/{id} is one operation instead of
// one per product.
func tracing(next http.Handler) http.Handler {
	return otelhttp.NewHandler(next, "",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			if route := chi.RouteContext(r.Context()); route != nil && route.RoutePattern() != "" {
				return r.Method + " " + route.RoutePattern()
			}
			return r.Method + " " + r.URL.Path
		}),
	)
}

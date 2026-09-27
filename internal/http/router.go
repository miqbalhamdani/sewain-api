package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
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
func NewRouter(srv ServerInterface, signer *auth.Signer) http.Handler {
	r := chi.NewRouter()
	// Outermost, so a span exists before anything can fail. An error raised by
	// the authentication middleware still carries a trace id that resolves.
	r.Use(tracing, Authenticate(signer))
	return HandlerFromMuxWithBaseURL(srv, r, BasePath)
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

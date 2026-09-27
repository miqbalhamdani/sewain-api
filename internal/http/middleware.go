package httpapi

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// Authenticate reads the bearer token and puts the owner it names into the
// request context.
//
// This is the only place an owner enters the system. API spec.md 1: derived
// from the token, never from a header, query parameter or body -- accepting it
// from the request would make cross-owner access a matter of editing one.
//
// Routes that opt out of authentication in openapi.yaml (`security: []`) are
// listed in unauthenticated below. Everything else needs a token, and a route
// that forgets to say so fails closed: with no owner in the context,
// InOwnerTx returns ErrNoOwnerContext rather than reading anything.
func Authenticate(signer *auth.Signer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if unauthenticated(r) {
				next.ServeHTTP(w, r)
				return
			}

			raw, ok := bearerToken(r)
			if !ok {
				writeError(w, r, apperrors.Unauthenticated("A bearer token is required."))
				return
			}
			claims, err := signer.Parse(raw)
			if err != nil {
				writeError(w, r, apperrors.Unauthenticated("The access token is invalid or has expired."))
				return
			}

			// Both come from the verified token and from nowhere else. The
			// role decides what the caller may do; the owner decides which
			// rows they may do it to.
			ctx := owner.NewContext(r.Context(), claims.OwnerID)
			ctx = auth.NewRoleContext(ctx, claims.Role)
			ctx = auth.NewUserContext(ctx, claims.UserID())

			// BR-092 wants owner_id on the span, and this is the first moment
			// it is known. Here rather than in InOwnerTx for two reasons: a
			// request that never touches the database still gets the
			// attribute, and one that runs twenty queries does not set it
			// twenty times. `tracing` runs before this middleware, so the
			// span in the context is the live server span.
			trace.SpanFromContext(ctx).SetAttributes(
				attribute.String("owner_id", claims.OwnerID.String()))

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// unauthenticated mirrors `security: []` in openapi.yaml.
//
// A hand-kept list is a real risk: adding an endpoint here by mistake would
// expose it. It is kept short and explicit for that reason, and the isolation
// suite covers every route regardless of which side of this it falls on.
func unauthenticated(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/api/v1/auth/login", "/api/v1/auth/refresh":
		return true
	default:
		return false
	}
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	// Case-insensitive: RFC 7235 makes the scheme name case-insensitive, and
	// clients do vary.
	if len(header) < 7 || !strings.EqualFold(header[:7], "bearer ") {
		return "", false
	}
	token := strings.TrimSpace(header[7:])
	return token, token != ""
}

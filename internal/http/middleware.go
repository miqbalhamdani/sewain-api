package httpapi

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
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
// marked noAuth in routeAccessTable below. Everything else needs a token, and a route
// that forgets to say so fails closed: with no owner in the context,
// InOwnerTx returns ErrNoOwnerContext rather than reading anything.
func Authenticate(signer *auth.Signer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if accessFor(r).noAuth {
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

// routeAccess is the one table that says how a route is gated.
//
// There used to be one hand-kept path list here for `security: []`. S1-084
// needs a second, different one for the verification gate -- and two lists
// that must be remembered together are two lists that will drift apart. One
// table with two columns cannot: a route appears once, and both properties are
// read from the same line.
//
// Absence is the safe answer for both columns. A route nobody added needs a
// token and needs a verified address, which is the failure mode you want from
// a list somebody forgot to update.
type routeAccess struct {
	// noAuth mirrors `security: []` in openapi.yaml.
	noAuth bool

	// preVerification is the BR-006 whitelist: reachable while
	// users.email_verified_at is still null. Exactly six, and each one is
	// either the way in, the way out, or the way to see which you are in.
	preVerification bool

	// idempotent marks the routes where Idempotency-Key is mandatory --
	// 04-api-spec.md §2.1 names eight, and every one of them either creates a
	// booking or moves money.
	//
	// None of them exist yet: the first is POST /bookings in S1-026. The
	// column is here rather than in a list of its own for the reason the
	// other two share this table -- three properties of one route belong on
	// one line, or they drift.
	idempotent bool
}

var routeAccessTable = map[string]routeAccess{
	// The four that must work before a session exists at all.
	"POST /api/v1/auth/login":             {noAuth: true, preVerification: true},
	"POST /api/v1/auth/refresh":           {noAuth: true, preVerification: true},
	"POST /api/v1/auth/register":          {noAuth: true, preVerification: true},
	"POST /api/v1/auth/verify-email":      {noAuth: true, preVerification: true},
	"POST /api/v1/auth/accept-invitation": {noAuth: true, preVerification: true},

	// Authenticated, but reachable before verifying. Logout is always allowed;
	// resend is the only way out of the wall; /me is how the frontend knows it
	// should be rendering the wall in the first place (BR-006).
	"POST /api/v1/auth/logout":              {preVerification: true},
	"POST /api/v1/auth/verify-email/resend": {preVerification: true},
	"GET /api/v1/me":                        {preVerification: true},

	// Idempotency-Key required (BR-090). Patterns use {id}; see accessFor.
	"POST /api/v1/bookings":             {idempotent: true}, // S1-026
	"POST /api/v1/bookings/{id}/pickup": {idempotent: true}, // S1-035
	"POST /api/v1/bookings/{id}/return": {idempotent: true}, // S1-036

	// M4: every POST that produces or releases money (BR-090).
	"POST /api/v1/bookings/{id}/deposit/settle": {idempotent: true}, // S1-042
	"POST /api/v1/bookings/{id}/deposit/waive":  {idempotent: true}, // S1-042
	"POST /api/v1/bookings/{id}/complete":       {idempotent: true}, // S1-042
	"POST /api/v1/invoices/{id}/payments":       {idempotent: true}, // S1-044
	"POST /api/v1/proofs/{id}/approve":          {idempotent: true}, // S1-046

	// Still to come from 04-api-spec.md §2.1, each with the item that adds the
	// route -- adding it without the flag is the mistake this list exists to
	// make visible:
	//
	//   S1-051  POST /public/bookings
	//           POST /invoices/{id}/lines   (discount lines, owner)
}

// accessFor looks a request up by its route pattern. Every path parameter in
// this API is a UUID, and these middlewares run before chi has routed (so
// RoutePattern is still empty) -- normalising UUID segments to {id} is the
// whole of the matching. A malformed id falls to the zero row: auth and
// verification still required, never idempotent, and the generated binder
// answers 400 after.
func accessFor(r *http.Request) routeAccess {
	segs := strings.Split(r.URL.Path, "/")
	for i, seg := range segs {
		if uuid.Validate(seg) == nil {
			segs[i] = "{id}"
		}
	}
	return routeAccessTable[r.Method+" "+strings.Join(segs, "/")]
}

// RequireVerifiedEmail is the gate on the whole backoffice (BR-006).
//
// One middleware, not a check per handler: a gate that has to be remembered at
// every endpoint is a gate that will be missed at the thirty-first. Everything
// outside routeAccessTable's preVerification column answers
// 403 email-not-verified while the address is unproved -- catalogue, bookings,
// handovers, invoices, all of it.
//
// It reads the claim rather than the database. The flag rides in the access
// token for the same reason owner_id and role do (BR-004), so verifying takes
// effect on the next token: POST /auth/verify-email is followed by
// /auth/refresh. One extra call on a once-per-account path, against one saved
// query on every request forever.
func RequireVerifiedEmail(signer *auth.Signer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if accessFor(r).preVerification {
				next.ServeHTTP(w, r)
				return
			}

			// Unauthenticated requests are already refused by Authenticate,
			// which runs first. Reaching here without a token means the route
			// is in neither column, and failing closed is correct.
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
			if !claims.EmailVerified {
				writeError(w, r, apperrors.EmailNotVerified())
				return
			}
			next.ServeHTTP(w, r)
		})
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

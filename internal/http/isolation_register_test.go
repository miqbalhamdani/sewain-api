package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Isolation cases for the registration and verification routes.  (S1-082, S1-084)
//
// Three of the four are unauthenticated, which does not exempt them -- it is
// the reason to look. A route that resolves an owner from something other than
// an access token is a route that can resolve the wrong one, and register
// creates the owner it answers with.
func init() {
	isolationCases = append(isolationCases,
		isolationCase{
			route: route{method: "POST", pattern: "/api/v1/auth/register"},
			seed:  seedAuthUser,
			request: func(t *testing.T, s seeded) *http.Request {
				// A fresh address: the column is unique globally, so reusing
				// the seeded one would test email-taken rather than isolation.
				email := "iso-reg-" + uuid.Must(uuid.NewV7()).String() + "@example.com"
				return jsonRequest(t, "/api/v1/auth/register", map[string]string{
					"email": email, "password": "isolation suite password",
					"business_name": "Iso Rental", "business_type": "vehicle_rental",
				})
			},
		},
		isolationCase{
			route: route{method: "POST", pattern: "/api/v1/auth/verify-email"},
			seed:  seedAuthUser,
			request: func(t *testing.T, s seeded) *http.Request {
				// An unredeemable token. The answer must be the same 422
				// whoever asks, and must not name a rental on the way out.
				return jsonRequest(t, "/api/v1/auth/verify-email", map[string]string{
					"token": "not-a-real-token",
				})
			},
		},
		isolationCase{
			route: route{method: "POST", pattern: "/api/v1/auth/verify-email/resend"},
			seed:  seedSignedInUser,
			request: func(t *testing.T, s seeded) *http.Request {
				return bearerRequest(t, http.MethodPost,
					"/api/v1/auth/verify-email/resend", s.accessToken)
			},
		},
		isolationCase{
			route: route{method: "POST", pattern: "/api/v1/auth/accept-invitation"},
			seed:  seedAuthUser,
			request: func(t *testing.T, s seeded) *http.Request {
				return jsonRequest(t, "/api/v1/auth/accept-invitation", map[string]string{
					"token": "not-a-real-token", "password": "isolation suite password",
				})
			},
		},
	)
}

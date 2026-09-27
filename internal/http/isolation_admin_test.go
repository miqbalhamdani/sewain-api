package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
)

// Isolation cases for the owner-only routes.  (S1-009, S1-010)
//
// These are the first routes in the system that can leak something other than
// a session: /settings carries the rental's slug, /users carries every address
// in it. Both run as owner A while owner B owns a row, and both assert B's
// marker never appears.
//
// The seeded user is an owner rather than an operator on purpose -- an
// operator would be refused at requirePermission and the case would pass
// without the handler ever running, which is a green test that proves nothing.
func init() {
	isolationCases = append(isolationCases,
		isolationCase{
			route: route{method: "GET", pattern: "/api/v1/settings"},
			seed:  seedSettingsOwner,
			request: func(t *testing.T, s seeded) *http.Request {
				return bearerRequest(t, http.MethodGet, "/api/v1/settings", s.accessToken)
			},
		},
		isolationCase{
			route: route{method: "PATCH", pattern: "/api/v1/settings"},
			seed:  seedSettingsOwner,
			request: func(t *testing.T, s seeded) *http.Request {
				return bodyRequest(t, http.MethodPatch, "/api/v1/settings", s.accessToken,
					`{"booking_code_prefix":"ISO"}`)
			},
		},
		isolationCase{
			route: route{method: "GET", pattern: "/api/v1/users"},
			seed:  seedSignedInOwner,
			request: func(t *testing.T, s seeded) *http.Request {
				return bearerRequest(t, http.MethodGet, "/api/v1/users", s.accessToken)
			},
		},
		isolationCase{
			route: route{method: "POST", pattern: "/api/v1/users"},
			seed:  seedSignedInOwner,
			request: func(t *testing.T, s seeded) *http.Request {
				// A fresh address every run: the column is unique globally, so
				// a fixed one would pass once and then collide forever.
				email := "iso-invite-" + uuid.Must(uuid.NewV7()).String() + "@example.com"
				return bodyRequest(t, http.MethodPost, "/api/v1/users", s.accessToken,
					`{"email":"`+email+`","name":"Iso Invitee"}`)
			},
		},
		isolationCase{
			route: route{method: "PATCH", pattern: "/api/v1/users/{id}"},
			seed:  seedSignedInOwner,
			request: func(t *testing.T, s seeded) *http.Request {
				// Owner B's own user id. Reaching it from A's token must be a
				// 404, and the marker must not come back either way.
				return bodyRequest(t, http.MethodPatch, "/api/v1/users/"+s.userID, s.accessToken,
					`{"status":"disabled"}`)
			},
		},
		isolationCase{
			route: route{method: "DELETE", pattern: "/api/v1/users/{id}"},
			seed:  seedSignedInOwner,
			request: func(t *testing.T, s seeded) *http.Request {
				return bearerRequest(t, http.MethodDelete, "/api/v1/users/"+s.userID, s.accessToken)
			},
		},
	)
}

// seedSignedInOwner seeds an owner-role user and logs them in.
//
// Role before login, not after: the role travels in the access token, so
// promoting the row afterwards would leave the token still saying operator and
// every case here would 403 without reaching a handler. A green test that
// never ran the code is worse than a red one.
func seedSignedInOwner(ctx context.Context, t *testing.T, store *db.Store, ownerID uuid.UUID) seeded {
	t.Helper()

	s := seedAuthUser(ctx, t, store, ownerID)

	if err := store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE users SET role = 'owner' WHERE id = $1`, s.userID)
		return err
	}); err != nil {
		t.Fatalf("promote to owner: %v", err)
	}

	signer, err := auth.NewSigner(isoSigningKey)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	session, err := auth.NewService(store, signer).Login(ctx, s.email, s.password)
	if err != nil {
		t.Fatalf("sign in seeded owner: %v", err)
	}
	s.accessToken = session.AccessToken
	s.refreshToken = session.RefreshToken
	return s
}

// seedSettingsOwner marks the fixture by its slug rather than its owner id.
//
// seedAuthUser already gives every rental a slug, and the slug is what a
// settings body actually carries -- checking for the owner id there would pass
// against a handler that returned somebody else's settings wholesale.
func seedSettingsOwner(ctx context.Context, t *testing.T, store *db.Store, ownerID uuid.UUID) seeded {
	t.Helper()
	s := seedSignedInOwner(ctx, t, store, ownerID)
	s.marker = "iso-" + ownerID.String()
	return s
}

func bodyRequest(t *testing.T, method, path, token, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

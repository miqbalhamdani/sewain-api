package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
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

// Isolation cases for the auth routes (S1-008).
//
// Every one of these seeds a real user at owner A and another at owner B,
// then acts as A. What must never come back is B's owner id, which appears in
// the Session body of a route that got the wrong owner.
//
// Login and refresh are unauthenticated, which does not exempt them: they are
// the two routes that resolve an owner from something other than an access
// token, so they are the two most able to resolve the wrong one.
func init() {
	isolationCases = append(isolationCases,
		isolationCase{
			route: route{method: "POST", pattern: "/api/v1/auth/login"},
			seed:  seedAuthUser,
			request: func(t *testing.T, s seeded) *http.Request {
				return jsonRequest(t, "/api/v1/auth/login", map[string]string{
					"email": s.email, "password": s.password,
				})
			},
		},
		isolationCase{
			route: route{method: "POST", pattern: "/api/v1/auth/refresh"},
			seed:  seedSignedInUser,
			request: func(t *testing.T, s seeded) *http.Request {
				r := jsonRequest(t, "/api/v1/auth/refresh", nil)
				r.AddCookie(&http.Cookie{Name: "refresh_token", Value: s.refreshToken})
				return r
			},
		},
		isolationCase{
			// GET /me is the sharpest case of the four: it exists to report an
			// owner id, so a route that resolved the wrong one says so out loud
			// in its body rather than leaking a row somewhere subtle.
			route: route{method: "GET", pattern: "/api/v1/me"},
			seed:  seedSignedInUser,
			request: func(t *testing.T, s seeded) *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
				r.Header.Set("Authorization", "Bearer "+s.accessToken)
				return r
			},
		},
		isolationCase{
			route: route{method: "POST", pattern: "/api/v1/auth/logout"},
			seed:  seedSignedInUser,
			request: func(t *testing.T, s seeded) *http.Request {
				r := jsonRequest(t, "/api/v1/auth/logout", nil)
				r.Header.Set("Authorization", "Bearer "+s.accessToken)
				r.AddCookie(&http.Cookie{Name: "refresh_token", Value: s.refreshToken})
				return r
			},
		},
	)
}

// seedAuthUser creates an owner and an active user in it.
func seedAuthUser(ctx context.Context, t *testing.T, store *db.Store, ownerID uuid.UUID) seeded {
	t.Helper()

	userID := uuid.Must(uuid.NewV7())
	email := fmt.Sprintf("iso-%s@example.com", userID)

	hash, err := auth.HashPassword(isoPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	ownerCtx := owner.NewContext(ctx, ownerID)
	if err := store.InOwnerTx(ownerCtx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO owners (id, name, slug) VALUES ($1, $2, $3)`,
			ownerID, "Isolation "+ownerID.String(), "iso-"+ownerID.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO users (id, owner_id, email, password_hash, name, role, status)
			 VALUES ($1, $2, $3, $4, 'Isolation user', 'operator', 'active')`,
			userID, ownerID, email, hash)
		return err
	}); err != nil {
		t.Fatalf("seed owner %s: %v", ownerID, err)
	}

	t.Cleanup(func() {
		cleanup := context.WithoutCancel(ctx)
		_ = store.InOwnerTx(owner.NewContext(cleanup, ownerID), func(tx pgx.Tx) error {
			if _, err := tx.Exec(cleanup, `DELETE FROM users WHERE id = $1`, userID); err != nil {
				return err
			}
			_, err := tx.Exec(cleanup, `DELETE FROM owners WHERE id = $1`, ownerID)
			return err
		})
	})

	// The owner id is the marker: it is what a Session body carries, so it is
	// what would appear if a route resolved the wrong owner.
	return seeded{marker: ownerID.String(), email: email, password: isoPassword,
		userID: userID.String()}
}

// seedSignedInUser is seedAuthUser plus a real login, for routes that need a
// live session to reach at all.
func seedSignedInUser(ctx context.Context, t *testing.T, store *db.Store, ownerID uuid.UUID) seeded {
	t.Helper()

	s := seedAuthUser(ctx, t, store, ownerID)

	signer, err := auth.NewSigner(isoSigningKey)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	session, err := auth.NewService(store, signer).Login(ctx, s.email, s.password)
	if err != nil {
		t.Fatalf("sign in seeded user: %v", err)
	}

	s.accessToken = session.AccessToken
	s.refreshToken = session.RefreshToken
	return s
}

func jsonRequest(t *testing.T, path string, body any) *http.Request {
	t.Helper()

	var payload string
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		payload = string(encoded)
	}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// bearerRequest builds an authenticated request. It lived in authz_test.go,
// which went away with the /roles endpoint; trace_test.go still needs it.
func bearerRequest(t *testing.T, method, path, token string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

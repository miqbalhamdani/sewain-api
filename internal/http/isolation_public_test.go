package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
)

// Isolation cases for M5 phase B and C: the public surface, the portal, and
// the API-key screen.  (S1-051, S1-053, S1-079)
//
// The public routes are reached the way a renter reaches them -- on the
// owner's own host, behind the proxy -- and owner B's marker is B's resource
// name for the catalogue and B's renter for the portal.
func init() {
	c := func(method, pattern string, seed func(context.Context, *testing.T, *db.Store, uuid.UUID) seeded,
		req func(t *testing.T, s seeded) *http.Request) isolationCase {
		return isolationCase{route: route{method: method, pattern: pattern}, seed: seed, request: req}
	}
	const later = `"start_at":"2030-01-10T09:00:00+07:00","end_at":"2030-01-12T09:00:00+07:00"`
	isolationCases = append(isolationCases,
		c("GET", "/api/v1/public/owner", seedPublicOwner, func(t *testing.T, s seeded) *http.Request {
			return tenantRequest(t, http.MethodGet, "/api/v1/public/owner", s.host, "")
		}),
		c("GET", "/api/v1/public/resources", seedPublicOwner, func(t *testing.T, s seeded) *http.Request {
			return tenantRequest(t, http.MethodGet,
				"/api/v1/public/resources?start_at=2030-01-10T09:00:00%2B07:00&end_at=2030-01-12T09:00:00%2B07:00", s.host, "")
		}),
		c("GET", "/api/v1/public/resources/{id}", seedPublicOwner, func(t *testing.T, s seeded) *http.Request {
			return tenantRequest(t, http.MethodGet, "/api/v1/public/resources/"+s.resourceID, s.host, "")
		}),
		c("POST", "/api/v1/public/bookings", seedPublicOwner, func(t *testing.T, s seeded) *http.Request {
			r := tenantRequest(t, http.MethodPost, "/api/v1/public/bookings", s.host,
				`{"resource_id":"`+s.resourceID+`",`+later+`,"customer":{"name":"Sari","phone":"081299990000"}}`)
			r.Header.Set("Idempotency-Key", uuid.Must(uuid.NewV7()).String())
			return r
		}),
		c("GET", "/api/v1/portal/bookings/{token}", seedPortalOwner, func(t *testing.T, s seeded) *http.Request {
			return tenantRequest(t, http.MethodGet, "/api/v1/portal/bookings/"+s.portalToken, s.host, "")
		}),
		c("POST", "/api/v1/portal/bookings/{token}/uploads", seedPortalOwner, func(t *testing.T, s seeded) *http.Request {
			return tenantRequest(t, http.MethodPost, "/api/v1/portal/bookings/"+s.portalToken+"/uploads", s.host,
				`{"content_type":"image/png","bytes":1000}`)
		}),
		c("POST", "/api/v1/portal/bookings/{token}/proofs", seedPortalOwner, func(t *testing.T, s seeded) *http.Request {
			return tenantRequest(t, http.MethodPost, "/api/v1/portal/bookings/"+s.portalToken+"/proofs", s.host,
				`{"invoice_id":"`+s.invoiceID+`","object_key":"pending/x"}`)
		}),
		c("GET", "/api/v1/api-keys", seedKeyOwner, func(t *testing.T, s seeded) *http.Request {
			return bearerRequest(t, http.MethodGet, "/api/v1/api-keys", s.accessToken)
		}),
		c("POST", "/api/v1/api-keys", seedKeyOwner, func(t *testing.T, s seeded) *http.Request {
			return bodyRequest(t, http.MethodPost, "/api/v1/api-keys", s.accessToken, `{"name":"situs uji"}`)
		}),
		c("DELETE", "/api/v1/api-keys/{id}", seedKeyOwner, func(t *testing.T, s seeded) *http.Request {
			return bearerRequest(t, http.MethodDelete, "/api/v1/api-keys/"+uuid.NewString(), s.accessToken)
		}),
	)
}

// seedPublicOwner is seedBookingOwner with a live public page (BR-096): slug
// (already iso-<id>), WhatsApp and address. Its marker is the resource name.
func seedPublicOwner(ctx context.Context, t *testing.T, store *db.Store, ownerID uuid.UUID) seeded {
	s := seedPortalOwner(ctx, t, store, ownerID)
	s.marker = "iso-resource-" + ownerID.String()
	return s
}

// seedPortalOwner is the same rental, marked by its renter's name: what the
// portal must never show another rental.
func seedPortalOwner(ctx context.Context, t *testing.T, store *db.Store, ownerID uuid.UUID) seeded {
	t.Helper()
	s := seedBookingOwner(ctx, t, store, ownerID)
	seedExec(ctx, t, store, ownerID,
		`UPDATE owners SET whatsapp = '+628123456789', address = 'Jl. Uji 1, Sleman' WHERE id = $1`, ownerID)
	s.host = "iso-" + ownerID.String() + "." + testApex
	s.portalToken = testPortal.Token(ownerID, uuid.MustParse(s.bookingID))
	return s
}

// seedKeyOwner is a signed-in owner whose keys are cleaned up before the owner
// row is (api_keys.created_by points at users).
func seedKeyOwner(ctx context.Context, t *testing.T, store *db.Store, ownerID uuid.UUID) seeded {
	t.Helper()
	s := seedSignedInOwner(ctx, t, store, ownerID)
	s.marker = "key-of-" + ownerID.String()
	seedExec(ctx, t, store, ownerID, `INSERT INTO api_keys (id, owner_id, name, key_prefix, key_hash)
		VALUES ($1, $2, $3, $4, 'x')`, uuid.Must(uuid.NewV7()), ownerID, s.marker, keyPrefixFor(ownerID))
	t.Cleanup(func() {
		c := context.WithoutCancel(ctx)
		_ = store.InOwnerTx(owner.NewContext(c, ownerID), func(tx pgx.Tx) error {
			_, err := tx.Exec(c, `DELETE FROM api_keys WHERE owner_id = $1`, ownerID)
			return err
		})
	})
	return s
}

// keyPrefixFor gives each seeded owner a distinct, valid 8-char prefix.
func keyPrefixFor(id uuid.UUID) string {
	return strings.ToLower(strings.ReplaceAll(id.String(), "-", ""))[24:32]
}

// tenantRequest is a request as Caddy forwards it: the tenant's Host, the
// proxy secret, and a client IP of its own so per-IP limits never couple tests.
func tenantRequest(t *testing.T, method, path, host, body string) *http.Request {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Host = host
	r.Header.Set("X-Proxy-Secret", testProxySecret)
	b := uuid.New()
	r.Header.Set("X-Real-IP", fmt.Sprintf("10.%d.%d.%d", b[0], b[1], b[2]))
	return r
}

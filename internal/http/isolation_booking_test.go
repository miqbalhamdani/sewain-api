package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
)

// Isolation cases for M2: customers, bookings, availability, calendar.
// (S1-020 .. S1-027, S1-078)
//
// The marker is the customer's name for every route that prints one -- a
// renter's name and phone are the most personal thing this system holds -- and
// the unit code for availability, which prints units but never renters.
func init() {
	c := func(method, pattern string, seed func(context.Context, *testing.T, *db.Store, uuid.UUID) seeded,
		req func(t *testing.T, s seeded) *http.Request) isolationCase {
		return isolationCase{route: route{method: method, pattern: pattern}, seed: seed, request: req}
	}
	isolationCases = append(isolationCases,
		c("GET", "/api/v1/customers", seedBookingOwner, func(t *testing.T, s seeded) *http.Request {
			return bearerRequest(t, http.MethodGet, "/api/v1/customers", s.accessToken)
		}),
		c("POST", "/api/v1/customers", seedBookingOwner, func(t *testing.T, s seeded) *http.Request {
			return bodyRequest(t, http.MethodPost, "/api/v1/customers", s.accessToken,
				`{"name":"Iso Baru","phone":"081200000000"}`)
		}),
		c("GET", "/api/v1/customers/{id}", seedBookingOwner, func(t *testing.T, s seeded) *http.Request {
			return bearerRequest(t, http.MethodGet, "/api/v1/customers/"+s.customerID, s.accessToken)
		}),
		c("PATCH", "/api/v1/customers/{id}", seedBookingOwner, func(t *testing.T, s seeded) *http.Request {
			return bodyRequest(t, http.MethodPatch, "/api/v1/customers/"+s.customerID, s.accessToken,
				`{"phone":"081299999999"}`)
		}),
		c("POST", "/api/v1/customers/{id}/blacklist", seedBookingOwner, func(t *testing.T, s seeded) *http.Request {
			return bodyRequest(t, http.MethodPost, "/api/v1/customers/"+s.customerID+"/blacklist",
				s.accessToken, `{"reason":"uji isolasi"}`)
		}),
		c("DELETE", "/api/v1/customers/{id}/blacklist", seedBookingOwner, func(t *testing.T, s seeded) *http.Request {
			return bearerRequest(t, http.MethodDelete, "/api/v1/customers/"+s.customerID+"/blacklist", s.accessToken)
		}),
		c("GET", "/api/v1/availability", seedBookingOwnerByUnit, func(t *testing.T, s seeded) *http.Request {
			return bearerRequest(t, http.MethodGet,
				"/api/v1/availability?start_at=2027-01-01T09:00:00%2B07:00&end_at=2027-01-03T09:00:00%2B07:00",
				s.accessToken)
		}),
		c("GET", "/api/v1/calendar", seedBookingOwner, func(t *testing.T, s seeded) *http.Request {
			return bearerRequest(t, http.MethodGet,
				"/api/v1/calendar?from=2026-09-01T00:00:00%2B07:00&to=2026-10-01T00:00:00%2B07:00",
				s.accessToken)
		}),
		c("GET", "/api/v1/bookings", seedBookingOwner, func(t *testing.T, s seeded) *http.Request {
			return bearerRequest(t, http.MethodGet, "/api/v1/bookings", s.accessToken)
		}),
		c("POST", "/api/v1/bookings", seedBookingOwner, func(t *testing.T, s seeded) *http.Request {
			r := bodyRequest(t, http.MethodPost, "/api/v1/bookings", s.accessToken,
				`{"customer_id":"`+s.customerID+`","resource_unit_id":"`+s.unitID+`",`+
					`"start_at":"2027-02-01T09:00:00+07:00","end_at":"2027-02-02T09:00:00+07:00"}`)
			r.Header.Set("Idempotency-Key", uuid.Must(uuid.NewV7()).String())
			return r
		}),
		c("GET", "/api/v1/bookings/{id}", seedBookingOwner, func(t *testing.T, s seeded) *http.Request {
			return bearerRequest(t, http.MethodGet, "/api/v1/bookings/"+s.bookingID, s.accessToken)
		}),
		c("PATCH", "/api/v1/bookings/{id}", seedBookingOwner, func(t *testing.T, s seeded) *http.Request {
			return bodyRequest(t, http.MethodPatch, "/api/v1/bookings/"+s.bookingID, s.accessToken,
				`{"resource_unit_id":"`+s.unitID+`"}`)
		}),
		c("POST", "/api/v1/bookings/{id}/confirm", seedBookingOwner, func(t *testing.T, s seeded) *http.Request {
			return bearerRequest(t, http.MethodPost, "/api/v1/bookings/"+s.bookingID+"/confirm", s.accessToken)
		}),
		c("POST", "/api/v1/bookings/{id}/cancel", seedBookingOwner, func(t *testing.T, s seeded) *http.Request {
			return bearerRequest(t, http.MethodPost, "/api/v1/bookings/"+s.bookingID+"/cancel", s.accessToken)
		}),
	)
}

// seedBookingOwner gives a rental the catalogue fixture plus one customer and
// one reserved booking on its unit, 3-5 Sep 2026.
func seedBookingOwner(ctx context.Context, t *testing.T, store *db.Store, ownerID uuid.UUID) seeded {
	t.Helper()
	s := seedCatalogOwner(ctx, t, store, ownerID)

	customerID, bookingID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	name := "iso-customer-" + ownerID.String()
	unitID, resourceID := uuid.MustParse(s.unitID), uuid.MustParse(s.resourceID)

	if err := store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO customers (id, owner_id, name, phone) VALUES ($1, $2, $3, '081234567890')`,
			customerID, ownerID, name); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO bookings (id, owner_id, code, customer_id, resource_id, resource_unit_id,
			                       start_at, end_at, status, unit_price, pricing_unit,
			                       duration_qty, subtotal)
			 VALUES ($1, $2, 'ISO-0001', $3, $4, $5, '2026-09-03 09:00+07', '2026-09-05 09:00+07',
			         'reserved', 350000, 'day', 2, 700000)`,
			bookingID, ownerID, customerID, resourceID, unitID)
		return err
	}); err != nil {
		t.Fatalf("seed bookings for owner %s: %v", ownerID, err)
	}

	t.Cleanup(func() {
		cleanup := context.WithoutCancel(ctx)
		_ = store.InOwnerTx(owner.NewContext(cleanup, ownerID), func(tx pgx.Tx) error {
			for _, q := range []string{
				`DELETE FROM bookings WHERE owner_id = $1`,
				`DELETE FROM booking_counters WHERE owner_id = $1`,
				`DELETE FROM customers WHERE owner_id = $1`,
			} {
				if _, err := tx.Exec(cleanup, q, ownerID); err != nil {
					return err
				}
			}
			return nil
		})
	})

	s.marker = name
	s.customerID = customerID.String()
	s.bookingID = bookingID.String()
	return s
}

// seedBookingOwnerByUnit is the same fixture marked by the unit code, for the
// one route that prints units and never renters.
func seedBookingOwnerByUnit(ctx context.Context, t *testing.T, store *db.Store, ownerID uuid.UUID) seeded {
	s := seedBookingOwner(ctx, t, store, ownerID)
	s.marker = "ISO-" + ownerID.String()
	return s
}

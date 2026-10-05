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

// Isolation cases for M5 phase A: dashboard, reports, export, jobs.
// (S1-056, S1-057, S1-063)
//
// The revenue report and the export answer prints no name or code, so their
// marker cannot appear either way; the sums are pinned per rental by
// booking.TestReports, which seeds a second rental's money beside the first.
func init() {
	c := func(method, pattern string, seed func(context.Context, *testing.T, *db.Store, uuid.UUID) seeded,
		req func(t *testing.T, s seeded) *http.Request) isolationCase {
		return isolationCase{route: route{method: method, pattern: pattern}, seed: seed, request: req}
	}
	const period = "from=2026-09-01T00:00:00%2B07:00&to=2026-10-01T00:00:00%2B07:00"
	isolationCases = append(isolationCases,
		c("GET", "/api/v1/dashboard", seedOverdueOwner, func(t *testing.T, s seeded) *http.Request {
			return bearerRequest(t, http.MethodGet, "/api/v1/dashboard", s.accessToken)
		}),
		c("GET", "/api/v1/reports/revenue", seedBookingOwner, func(t *testing.T, s seeded) *http.Request {
			return bearerRequest(t, http.MethodGet, "/api/v1/reports/revenue?"+period, s.accessToken)
		}),
		c("GET", "/api/v1/reports/utilization", seedBookingOwnerByUnit, func(t *testing.T, s seeded) *http.Request {
			return bearerRequest(t, http.MethodGet, "/api/v1/reports/utilization?"+period, s.accessToken)
		}),
		c("GET", "/api/v1/reports/idle-units", seedIdleOwner, func(t *testing.T, s seeded) *http.Request {
			return bearerRequest(t, http.MethodGet, "/api/v1/reports/idle-units", s.accessToken)
		}),
		c("POST", "/api/v1/reports/export", seedBookingOwner, func(t *testing.T, s seeded) *http.Request {
			return bodyRequest(t, http.MethodPost, "/api/v1/reports/export", s.accessToken,
				`{"report":"bookings","format":"csv","from":"2026-09-01T00:00:00+07:00","to":"2026-10-01T00:00:00+07:00"}`)
		}),
		// A job id is a Redis key, not a row: another rental's id answers 404
		// (jobs.TestTrackedJob), and so does one that never existed.
		c("GET", "/api/v1/jobs/{id}", seedBookingOwner, func(t *testing.T, s seeded) *http.Request {
			return bearerRequest(t, http.MethodGet, "/api/v1/jobs/"+uuid.NewString(), s.accessToken)
		}),
	)
}

// seedOverdueOwner puts the seeded booking out and past its end, so the
// dashboard's overdue list prints the renter's name.
func seedOverdueOwner(ctx context.Context, t *testing.T, store *db.Store, ownerID uuid.UUID) seeded {
	s := seedBookingOwner(ctx, t, store, ownerID)
	seedExec(ctx, t, store, ownerID, `UPDATE bookings SET status = 'picked_up' WHERE id = $1`, s.bookingID)
	return s
}

// seedIdleOwner ages the seeded unit past BR-075's 30 days, never rented, so
// the idle list prints its code.
func seedIdleOwner(ctx context.Context, t *testing.T, store *db.Store, ownerID uuid.UUID) seeded {
	s := seedBookingOwnerByUnit(ctx, t, store, ownerID)
	seedExec(ctx, t, store, ownerID, `UPDATE resource_units SET created_at = now() - interval '60 days' WHERE id = $1`, s.unitID)
	return s
}

func seedExec(ctx context.Context, t *testing.T, store *db.Store, ownerID uuid.UUID, sql string, args ...any) {
	t.Helper()
	if err := store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	}); err != nil {
		t.Fatalf("seed %q: %v", sql, err)
	}
}

package booking

import (
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
)

// PRD section 9 and S1-025: availability over 500 units x 12 months answers at
// p95 under a second. The calendar is held to the same budget at that scale,
// which covers S1-028's much smaller 50 x 30 first paint.
//
// Seeded with roughly one booking per unit per fortnight -- 12,000 rows, a
// busy year -- so the NOT EXISTS probe has real ranges to walk.
func TestAvailabilityAndCalendarP95(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 12,000 bookings")
	}
	f := newFixture(t, 500)
	f.exec(t, `
		INSERT INTO bookings (id, owner_id, code, customer_id, resource_id, resource_unit_id,
		                      start_at, end_at, status, unit_price, pricing_unit, duration_qty, subtotal)
		SELECT gen_random_uuid(), $1, 'P-' || row_number() OVER (), $2, $3, u.id,
		       '2027-01-01'::timestamptz + (w * 14 || ' days')::interval,
		       '2027-01-01'::timestamptz + (w * 14 + 3 || ' days')::interval,
		       CASE WHEN w % 5 = 0 THEN 'picked_up' ELSE 'reserved' END, 1, 'day', 3, 3
		  FROM resource_units u, generate_series(0, 23) w`, f.ownerID, f.customer, f.resource)

	// As the table owner: app_user owns nothing and ANALYZE skips what it does
	// not own. A populated table in production has stats from autovacuum; a
	// freshly seeded one has none, the planner estimates one row everywhere,
	// and the number measured would be a plan nobody ever runs.
	ownerConn, err := pgx.Connect(t.Context(), config.DatabaseURL())
	if err != nil {
		t.Fatalf("connect as schema owner: %v", err)
	}
	defer func() { _ = ownerConn.Close(t.Context()) }()
	if _, err := ownerConn.Exec(t.Context(), `ANALYZE bookings; ANALYZE resource_units; ANALYZE resources`); err != nil {
		t.Fatal(err)
	}

	s := New(f.store)
	p95 := func(name string, run func() error) {
		var took []time.Duration
		for range 20 {
			began := time.Now()
			if err := run(); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			took = append(took, time.Since(began))
		}
		slices.Sort(took)
		p := took[len(took)*95/100-1]
		t.Logf("%s p95 = %v", name, p)
		if p > time.Second {
			t.Errorf("%s p95 = %v, budget is 1s", name, p)
		}
	}

	from := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	p95("availability, 2 days", func() error {
		_, err := s.Availability(f.ctx, from.AddDate(0, 3, 1), from.AddDate(0, 3, 3), nil)
		return err
	})
	p95("availability, 12 months", func() error {
		_, err := s.Availability(f.ctx, from, from.AddDate(1, 0, 0), &f.resource)
		return err
	})
	p95("calendar, 500 units x 12 months", func() error {
		_, err := s.Calendar(f.ctx, from, from.AddDate(1, 0, 0), time.Now())
		return err
	})
}

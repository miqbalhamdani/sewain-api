package booking

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
)

// fixture is one rental with a daily-priced resource, `units` units and one
// customer, against the real database as app_user -- no mocks (CLAUDE.md).
type fixture struct {
	store    *db.Store
	ctx      context.Context // carries the owner
	ownerID  uuid.UUID
	userID   uuid.UUID
	resource uuid.UUID
	units    []uuid.UUID
	customer uuid.UUID
}

func newFixture(t *testing.T, units int) fixture {
	t.Helper()
	store, err := db.New(t.Context(), config.AppDatabaseURL())
	if err != nil {
		t.Fatalf("connect as app_user: %v -- is PostgreSQL running and migrated?", err)
	}
	t.Cleanup(store.Close)

	f := fixture{store: store, ownerID: uuid.Must(uuid.NewV7()), userID: uuid.Must(uuid.NewV7()),
		resource: uuid.Must(uuid.NewV7()), customer: uuid.Must(uuid.NewV7())}
	f.ctx = owner.NewContext(t.Context(), f.ownerID)

	f.exec(t, `INSERT INTO owners (id, name, slug, business_type) VALUES ($1, 'Fixture', $2, 'vehicle_rental')`,
		f.ownerID, "fx-"+f.ownerID.String())
	f.exec(t, `INSERT INTO users (id, owner_id, email, password_hash, name, role, status, email_verified_at)
	           VALUES ($1, $2, $3, 'x', 'Fixture', 'owner', 'active', now())`,
		f.userID, f.ownerID, fmt.Sprintf("fx-%s@example.com", f.userID))
	f.exec(t, `INSERT INTO resources (id, owner_id, name, pricing_unit, base_price) VALUES ($1, $2, 'Avanza', 'day', 350000)`,
		f.resource, f.ownerID)
	f.exec(t, `INSERT INTO customers (id, owner_id, name, phone) VALUES ($1, $2, 'Budi', '0812')`,
		f.customer, f.ownerID)
	f.exec(t, `INSERT INTO resource_units (id, owner_id, resource_id, code)
	           SELECT gen_random_uuid(), $1, $2, 'FX-' || g FROM generate_series(1, $3::int) g`,
		f.ownerID, f.resource, units)
	if err := store.InOwnerTx(f.ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(f.ctx, `SELECT id FROM resource_units ORDER BY code`)
		if err != nil {
			return err
		}
		f.units, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	}); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		ctx := context.WithoutCancel(f.ctx)
		_ = store.InOwnerTx(ctx, func(tx pgx.Tx) error {
			for _, q := range []string{
				`DELETE FROM bookings`, `DELETE FROM booking_counters`, `DELETE FROM customers`,
				`DELETE FROM resource_units`, `DELETE FROM resources`, `DELETE FROM users`,
				`DELETE FROM owners WHERE id = $1`,
			} {
				var err error
				if q == `DELETE FROM owners WHERE id = $1` {
					_, err = tx.Exec(ctx, q, f.ownerID)
				} else {
					_, err = tx.Exec(ctx, q)
				}
				if err != nil {
					return err
				}
			}
			return nil
		})
	})
	return f
}

func (f fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if err := f.store.InOwnerTx(f.ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.ctx, sql, args...)
		return err
	}); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

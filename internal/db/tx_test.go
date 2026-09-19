package db

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/owner"
	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
)

// TestInOwnerTx is P1-007's acceptance criterion: a missing owner returns
// ErrNoOwnerContext, never an empty result.
//
// Both halves of that sentence are tested. "Returns an error" is easy and would
// pass even if row level security were doing nothing at all -- so the isolation
// subtest asserts a specific non-zero row count, which fails both when RLS is
// broken open and when it is broken shut.
//
// Every connection under test is app_user. The owning role is a superuser
// locally, and a superuser bypasses RLS entirely; a test written against it
// would prove nothing.
func TestInOwnerTx(t *testing.T) {
	ctx := t.Context()

	store, err := New(ctx, config.AppDatabaseURL())
	if err != nil {
		t.Fatalf("connect as app_user: %v\n\nIs PostgreSQL running and migrated?\n"+
			"  brew services start postgresql@18\n  make db-create && make migrate", err)
	}
	t.Cleanup(store.Close)

	// Schema changes need the owner; app_user owns nothing and cannot make them.
	ownerConn, err := pgx.Connect(ctx, config.DatabaseURL())
	if err != nil {
		t.Fatalf("connect as owner: %v", err)
	}
	t.Cleanup(func() { _ = ownerConn.Close(ctx) })

	table := createOwnerTable(ctx, t, ownerConn, "tx_scratch")
	ownerA, ownerB := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())

	// The acceptance, literally.
	t.Run("missing owner returns ErrNoOwnerContext", func(t *testing.T) {
		called := false
		err := store.InOwnerTx(ctx, func(pgx.Tx) error {
			called = true
			return nil
		})

		if !errors.Is(err, ErrNoOwnerContext) {
			t.Fatalf("got %v, want ErrNoOwnerContext", err)
		}
		// The guard runs before a connection is taken and before any query is
		// sent. If the callback ran, the refusal is not a refusal.
		if called {
			t.Error("callback ran despite there being no owner")
		}
	})

	t.Run("an owner-scoped write succeeds", func(t *testing.T) {
		for _, id := range []uuid.UUID{ownerA, ownerB} {
			err := store.InOwnerTx(owner.NewContext(ctx, id), func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx,
					fmt.Sprintf(`INSERT INTO %s (owner_id, note) VALUES ($1, $2)`, table),
					id, "row for "+id.String())
				return err
			})
			if err != nil {
				t.Fatalf("insert for %s: %v", id, err)
			}
		}
	})

	// The other half of the acceptance, and the harder claim: not merely that a
	// missing owner errors, but that a present one actually filters.
	t.Run("an owner sees only its own rows", func(t *testing.T) {
		var count int
		err := store.InOwnerTx(owner.NewContext(ctx, ownerA), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&count)
		})
		if err != nil {
			t.Fatalf("count as owner A: %v", err)
		}
		switch count {
		case 1:
			// Correct: two rows exist, one belongs to this owner.
		case 0:
			t.Error("owner A sees 0 rows -- the policy is filtering everything, " +
				"which is the silent-empty failure ErrNoOwnerContext exists to prevent")
		default:
			t.Errorf("owner A sees %d rows, want 1 -- row level security is not filtering", count)
		}
	})

	t.Run("the write policy rejects another owner's row", func(t *testing.T) {
		err := store.InOwnerTx(owner.NewContext(ctx, ownerA), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				fmt.Sprintf(`INSERT INTO %s (owner_id, note) VALUES ($1, $2)`, table),
				ownerB, "forged")
			return err
		})
		if err == nil {
			t.Fatal("owner A wrote a row owned by owner B; WITH CHECK is not enforcing")
		}
	})

	t.Run("a failing callback rolls back", func(t *testing.T) {
		sentinel := errors.New("deliberate")
		err := store.InOwnerTx(owner.NewContext(ctx, ownerA), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx,
				fmt.Sprintf(`INSERT INTO %s (owner_id, note) VALUES ($1, $2)`, table),
				ownerA, "should not survive"); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("got %v, want the callback's own error unwrapped", err)
		}

		var count int
		if err := store.InOwnerTx(owner.NewContext(ctx, ownerA), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				fmt.Sprintf(`SELECT count(*) FROM %s WHERE note = 'should not survive'`, table),
			).Scan(&count)
		}); err != nil {
			t.Fatalf("count after rollback: %v", err)
		}
		if count != 0 {
			t.Errorf("%d row(s) survived a failed callback, want 0", count)
		}
	})

	// The specific bug tdd.md 3.2 warns about. is_local = true is one word in
	// one line, it is invisible if untested, and getting it wrong serves one
	// owner's rows to the next request that happens to reuse the connection.
	t.Run("the owner setting does not outlive its transaction", func(t *testing.T) {
		// One connection in the pool, so the next query is guaranteed to reuse
		// the same backend the transaction just used.
		pinned, err := New(ctx, config.AppDatabaseURL()+"&pool_max_conns=1")
		if err != nil {
			t.Fatalf("open single-connection pool: %v", err)
		}
		t.Cleanup(pinned.Close)

		if err := pinned.InOwnerTx(owner.NewContext(ctx, ownerA), func(tx pgx.Tx) error {
			var got string
			// Sanity: the setting is visible inside the transaction, so a blank
			// reading afterwards means it was cleared, not never set.
			if err := tx.QueryRow(ctx,
				`SELECT current_setting('app.owner_id', true)`).Scan(&got); err != nil {
				return err
			}
			if got != ownerA.String() {
				t.Errorf("inside the transaction app.owner_id is %q, want %q", got, ownerA)
			}
			return nil
		}); err != nil {
			t.Fatalf("transaction: %v", err)
		}

		// Reached directly rather than through an exported helper: this is the
		// pooled connection's session state, which is not something the rest of
		// the application has any business asking about.
		var leaked string
		if err := pinned.pool.QueryRow(ctx,
			`SELECT current_setting('app.owner_id', true)`).Scan(&leaked); err != nil {
			t.Fatalf("read setting after commit: %v", err)
		}
		if leaked != "" {
			t.Errorf("app.owner_id is still %q on the pooled connection after commit -- "+
				"the setting is session-scoped and will follow this connection into the "+
				"next request's owner", leaked)
		}
	})
}

// createOwnerTable builds an owner-owned table and protects it with the helper
// from P1-006, then drops it when the test ends.
func createOwnerTable(ctx context.Context, t *testing.T, owner *pgx.Conn, name string) string {
	t.Helper()

	drop := fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name)
	if _, err := owner.Exec(ctx, drop); err != nil {
		t.Fatalf("drop stale %s: %v", name, err)
	}
	if _, err := owner.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %s (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), owner_id uuid NOT NULL, note text)`,
		name)); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if _, err := owner.Exec(ctx, `SELECT enable_owner_rls($1)`, name); err != nil {
		t.Fatalf("enable_owner_rls(%s): %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := owner.Exec(context.WithoutCancel(ctx), drop); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	return name
}

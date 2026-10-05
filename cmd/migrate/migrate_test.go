package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
)

// TestBootstrapMigration is P1-006's acceptance criterion: app_user owns
// nothing, and FORCE RLS holds on every owner table.
//
// Like the P1-000 health test it fails rather than skips when PostgreSQL is
// unreachable -- a check suite that goes green without ever having touched a
// database proves nothing about a migration.
func TestBootstrapMigration(t *testing.T) {
	ctx := t.Context()

	if err := run(0); err != nil {
		t.Fatalf("apply migrations: %v\n\nIs PostgreSQL running and the database created?\n"+
			"  brew services start postgresql@18\n  make db-create", err)
	}

	conn, err := pgx.Connect(ctx, config.DatabaseURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })

	t.Run("app_user exists and can log in", func(t *testing.T) {
		var canLogin, bypassRLS, superuser bool
		err := conn.QueryRow(ctx,
			`SELECT rolcanlogin, rolbypassrls, rolsuper FROM pg_roles WHERE rolname = 'app_user'`,
		).Scan(&canLogin, &bypassRLS, &superuser)
		if errors.Is(err, pgx.ErrNoRows) {
			t.Fatal("app_user does not exist")
		}
		if err != nil {
			t.Fatalf("read pg_roles: %v", err)
		}
		if !canLogin {
			t.Error("app_user cannot log in; the application could not connect as it")
		}
		// Either of these would let the application read every owner's rows
		// regardless of any policy.
		if bypassRLS {
			t.Error("app_user has BYPASSRLS -- row level security would not apply to it")
		}
		if superuser {
			t.Error("app_user is a superuser -- row level security would not apply to it")
		}
	})

	// The acceptance, first clause. FORCE RLS binds the table owner, so the
	// guarantee only holds while the application's role owns nothing.
	t.Run("app_user owns nothing", func(t *testing.T) {
		var owned []string
		rows, err := conn.Query(ctx, `
			SELECT 'relation ' || c.relname
			  FROM pg_class c
			  JOIN pg_roles r ON r.oid = c.relowner
			 WHERE r.rolname = 'app_user'
			UNION ALL
			SELECT 'function ' || p.proname
			  FROM pg_proc p
			  JOIN pg_roles r ON r.oid = p.proowner
			 WHERE r.rolname = 'app_user'
			UNION ALL
			SELECT 'schema ' || n.nspname
			  FROM pg_namespace n
			  JOIN pg_roles r ON r.oid = n.nspowner
			 WHERE r.rolname = 'app_user'`)
		if err != nil {
			t.Fatalf("query owned objects: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("scan: %v", err)
			}
			owned = append(owned, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate: %v", err)
		}
		if len(owned) > 0 {
			t.Errorf("app_user owns %d object(s), want 0: %s\n"+
				"FORCE RLS does not protect against a table's own owner reaching past it.",
				len(owned), strings.Join(owned, ", "))
		}
	})

	// The acceptance, second clause. Vacuous while no owner table exists --
	// P1-010 is the first -- so the subtest below is what gives it teeth today.
	//
	// The query itself moved to db.CheckOwnerRLS in P1-009, which is also what
	// `make lint-rls` runs. One copy, so this test and the guard can never
	// disagree about what "protected" means.
	t.Run("every owner table has forced RLS and a policy", func(t *testing.T) {
		store, err := db.New(ctx, config.DatabaseURL())
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(store.Close)

		problems, err := store.CheckOwnerRLS(ctx)
		if err != nil {
			t.Fatalf("check owner RLS: %v", err)
		}
		for _, p := range problems {
			// Other packages' tests make deliberately unprotected owner tables
			// named *scratch* while this one runs (go test runs packages in
			// parallel against one database). They are not the schema.
			if strings.Contains(p.Table, "scratch") {
				continue
			}
			t.Error(p)
		}
	})

	// What keeps the clause above from being an empty assertion today: build a
	// owner table, hand it to the helper, and check all three properties the
	// helper is supposed to set.
	t.Run("enable_owner_rls sets all three properties", func(t *testing.T) {
		table := createScratchTable(ctx, t, conn, "owner_id uuid NOT NULL, name text")

		if _, err := conn.Exec(ctx, `SELECT enable_owner_rls($1)`, table); err != nil {
			t.Fatalf("enable_owner_rls(%s): %v", table, err)
		}

		var enabled, forced bool
		var policies int
		err := conn.QueryRow(ctx, `
			SELECT c.relrowsecurity, c.relforcerowsecurity,
			       (SELECT count(*) FROM pg_policy p
			         WHERE p.polrelid = c.oid AND p.polname = 'owner_isolation')
			  FROM pg_class c WHERE c.oid = $1::regclass`, table,
		).Scan(&enabled, &forced, &policies)
		if err != nil {
			t.Fatalf("read table flags: %v", err)
		}

		if !enabled {
			t.Error("RLS not enabled")
		}
		if !forced {
			t.Error("RLS not FORCEd")
		}
		if policies != 1 {
			t.Errorf("found %d owner_isolation policies, want 1", policies)
		}
	})

	t.Run("enable_owner_rls refuses a table with no owner_id", func(t *testing.T) {
		table := createScratchTable(ctx, t, conn, "name text")

		_, err := conn.Exec(ctx, `SELECT enable_owner_rls($1)`, table)
		if err == nil {
			t.Fatal("expected an error; a policy on owner_id would raise on every row instead")
		}
		if !strings.Contains(err.Error(), "no owner_id column") {
			t.Errorf("error does not explain the cause: %v", err)
		}
	})
}

// createScratchTable makes a table for one subtest and drops it afterwards.
func createScratchTable(ctx context.Context, t *testing.T, conn *pgx.Conn, columns string) string {
	t.Helper()

	// Unique per subtest so a leftover from a failed run cannot collide.
	name := fmt.Sprintf("rls_scratch_%d", len(t.Name()))
	if _, err := conn.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name)); err != nil {
		t.Fatalf("drop stale scratch table: %v", err)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (%s)`, name, columns)); err != nil {
		t.Fatalf("create scratch table: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name)); err != nil {
			t.Errorf("drop scratch table %s: %v", name, err)
		}
	})
	return name
}

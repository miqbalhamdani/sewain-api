package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
)

// TestLintRLS is P1-009's acceptance: the command exits non-zero on an owner
// table with no policy. main() exits 1 exactly when run() returns an error, so
// asserting on run() is asserting on the exit code.
//
// The order matters. The clean run has to pass first -- it is what proves a
// failure in the second half is the unprotected table rather than a database
// that was never reachable.
// probe follows the convention every test-made owner table here shares: its
// name contains "scratch", so schema-wide checks running in parallel can tell
// it from the schema under test.
const probe = "lint_rls_scratch"

func TestLintRLS(t *testing.T) {
	ctx := t.Context()

	conn, err := pgx.Connect(ctx, config.DatabaseURL())
	if err != nil {
		t.Fatalf("connect: %v\n\nIs PostgreSQL running and migrated?\n"+
			"  brew services start postgresql@18\n  make db-create && make migrate", err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })

	t.Run("passes on a clean schema", func(t *testing.T) {
		if ps := schemaProblems(ctx, t); len(ps) > 0 {
			t.Fatalf("problems = %v, want none -- the schema should be clean before this test adds to it", ps)
		}
	})

	// Each of the three ways an owner table can be wrong, including the two the
	// acceptance does not name. A table with no FORCE looks entirely healthy
	// from the application's side and leaks only to the owner.
	for _, tt := range []struct {
		name  string
		setup string
	}{
		{
			name:  "no policy at all",
			setup: ``,
		},
		{
			name: "RLS enabled but not FORCEd",
			setup: `ALTER TABLE %s ENABLE ROW LEVEL SECURITY;
			        CREATE POLICY owner_isolation ON %s USING (true)`,
		},
		{
			name: "RLS enabled and FORCEd but no policy",
			setup: `ALTER TABLE %s ENABLE ROW LEVEL SECURITY;
			        ALTER TABLE %s FORCE ROW LEVEL SECURITY`,
		},
	} {
		t.Run("fails when an owner table has "+tt.name, func(t *testing.T) {
			createUnprotected(ctx, t, conn, probe, tt.setup)

			if err := run(ctx); err == nil {
				t.Error("run() = nil, want an error -- lint-rls would have exited 0 " +
					"on an unprotected owner table")
			}
		})
	}

	t.Run("passes again once the table is protected", func(t *testing.T) {
		table := createUnprotected(ctx, t, conn, probe, ``)
		if _, err := conn.Exec(ctx, `SELECT enable_owner_rls($1)`, table); err != nil {
			t.Fatalf("enable_owner_rls: %v", err)
		}

		if ps := schemaProblems(ctx, t); len(ps) > 0 {
			t.Errorf("problems = %v, want none -- a properly protected table is being reported", ps)
		}
	})
}

// schemaProblems is CheckOwnerRLS without other tests' scratch tables -- but
// with this test's own probe.
//
// go test runs packages in parallel against one database, and cmd/migrate,
// internal/db and internal/http each create a deliberately unprotected owner
// table named *scratch* while they run. run() reports those too -- correctly
// -- so the two "should be clean" assertions here look only at the schema
// under test and at this test's own probe. The failing cases still go through
// run(), which is what proves the exit code. (CI's first run on main caught
// rls_scratch_65 from cmd/migrate in the middle of this test.)
func schemaProblems(ctx context.Context, t *testing.T) []db.RLSProblem {
	t.Helper()
	store, err := db.New(ctx, config.DatabaseURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer store.Close()
	all, err := store.CheckOwnerRLS(ctx)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	var out []db.RLSProblem
	for _, p := range all {
		if p.Table == probe || !strings.Contains(p.Table, "scratch") {
			out = append(out, p)
		}
	}
	return out
}

// createUnprotected makes an owner-owned table, optionally half-configuring its
// row level security, and drops it when the subtest ends.
func createUnprotected(ctx context.Context, t *testing.T, conn *pgx.Conn, name, setup string) string {
	t.Helper()

	drop := `DROP TABLE IF EXISTS ` + name
	if _, err := conn.Exec(ctx, drop); err != nil {
		t.Fatalf("drop stale %s: %v", name, err)
	}
	if _, err := conn.Exec(ctx,
		`CREATE TABLE `+name+` (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), owner_id uuid NOT NULL)`); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if setup != "" {
		if _, err := conn.Exec(ctx, fmt.Sprintf(setup, name, name)); err != nil {
			t.Fatalf("set up %s: %v", name, err)
		}
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), drop); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	return name
}

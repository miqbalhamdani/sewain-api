// Owner isolation suite (P1-008).
//
// Two things live here. The registry below lists, per route, how to reach one
// owner's row as another owner -- and the coverage check walks the routes the
// contract actually registers and fails when one has no entry. A new route
// without an isolation case is an incomplete route, and this is what says so
// rather than a reviewer having to notice.
//
// Right now the contract has no paths, so both lists are empty and the coverage
// check passes over nothing. TestIsolationHarness is what keeps that honest: it
// builds a route that leaks and proves the harness catches it, and a route that
// does not and proves it does not cry wolf.
//
// Two things swap when their items land. Auth is an owner on the request
// context here; from P1-011 it is a real token, and withOwner becomes the auth
// middleware. And routes are walked from a router this file builds, because
// cmd/api does not mount the generated one yet; when it does, walk that.
package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	httpapi "github.com/miqbalhamdani/sewain-api/internal/http"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
	"github.com/miqbalhamdani/sewain-api/internal/settings"
)

const (
	isoSigningKey = "isolation-suite-signing-key-32-bytes"
	isoPassword   = "isolation suite password"
)

// route is one method-and-pattern pair registered on the router.
type route struct {
	method  string
	pattern string
}

func (r route) String() string { return r.method + " " + r.pattern }

// seeded is what one owner's fixture leaves behind for the request builder.
//
// marker is the only required field: a string that would appear in a response
// that leaked this owner's data. The rest are credentials the route needs to
// be reached at all, and each case fills in only what it uses.
type seeded struct {
	marker       string
	email        string
	password     string
	accessToken  string
	refreshToken string

	// userID is the seeded user's own id, so a case can aim an id-taking route
	// at owner B's row and watch it answer 404 rather than 403.
	userID string
}

// isolationCase says how to exercise one route as owner A after owner B owns
// a row, so the suite can check that B's row does not come back.
type isolationCase struct {
	route   route
	seed    func(ctx context.Context, t *testing.T, store *db.Store, ownerID uuid.UUID) seeded
	request func(t *testing.T, s seeded) *http.Request
}

// isolationCases grows one entry per route, added by the item that adds the
// route. P1-011 auth, P1-021 brands, P1-024 categories, P1-028 products.
var isolationCases []isolationCase

// TestOwnerIsolation is P1-008's acceptance: two seeded owners, and owner A
// sees zero of owner B's rows on every registered route.
func TestOwnerIsolation(t *testing.T) {
	registered := registeredRoutes(t)

	// The guard that makes the rest of this file self-maintaining.
	t.Run("every registered route has an isolation case", func(t *testing.T) {
		missing := routesWithoutCase(registered, isolationCases)
		for _, r := range missing {
			t.Errorf("%s has no isolation case -- add one to isolationCases in this file", r)
		}
		t.Logf("%d registered route(s), %d case(s)", len(registered), len(isolationCases))
	})

	if len(isolationCases) == 0 {
		// Not a skip of the acceptance: TestIsolationHarness below proves the
		// machinery works on a route built for the purpose.
		t.Log("no routes in the contract yet; TestIsolationHarness covers the machinery")
		return
	}

	if newServer == nil {
		t.Fatal("isolationCases is not empty but newServer is nil -- " +
			"point it at the server cmd/api serves")
	}

	ctx := t.Context()
	store := openAppStore(ctx, t)
	srv := newServer(t)

	for _, c := range isolationCases {
		t.Run(c.route.String(), func(t *testing.T) {
			ownerA, ownerB := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
			a := c.seed(ctx, t, store, ownerA)
			b := c.seed(ctx, t, store, ownerB)

			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, c.request(t, a))

			assertNoLeak(t, rec, b.marker)
		})
	}
}

// TestIsolationHarness proves the suite above can fail. Without it, an empty
// case list and an empty route list would make TestOwnerIsolation a test that
// cannot report anything.
func TestIsolationHarness(t *testing.T) {
	ctx := t.Context()
	store := openAppStore(ctx, t)

	owner, err := pgx.Connect(ctx, config.DatabaseURL())
	if err != nil {
		t.Fatalf("connect as owner: %v", err)
	}
	t.Cleanup(func() { _ = owner.Close(ctx) })

	table := createOwnerTable(ctx, t, owner, "iso_scratch")
	ownerA, ownerB := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	seedRow(ctx, t, store, table, ownerA)
	markerB := seedRow(ctx, t, store, table, ownerB)

	t.Run("catches a route that leaks", func(t *testing.T) {
		// The realistic bug this models: a handler reaching for the owning
		// connection instead of the application's. The owner is a superuser
		// locally, superusers bypass row level security outright, and so the
		// owner filter silently does nothing.
		leaky := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			notes, err := allNotes(r.Context(), owner, table)
			if err != nil {
				t.Errorf("leaky handler: %v", err)
				return
			}
			_, _ = fmt.Fprint(w, strings.Join(notes, ","))
		})

		rec := httptest.NewRecorder()
		leaky.ServeHTTP(rec, requestAsOwner(t, ownerA))

		// Deliberately inverted: the harness is supposed to fail here.
		if !strings.Contains(rec.Body.String(), markerB) {
			t.Fatalf("the leaky handler did not leak, so this proves nothing; body=%q", rec.Body)
		}
		if leaked := findLeak(rec, markerB); leaked == "" {
			t.Error("assertNoLeak would have passed a response containing owner B's row")
		}
	})

	t.Run("passes a route that isolates", func(t *testing.T) {
		clean := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var notes []string
			err := store.InOwnerTx(r.Context(), func(tx pgx.Tx) error {
				var err error
				notes, err = allNotesTx(r.Context(), tx, table)
				return err
			})
			if err != nil {
				t.Errorf("clean handler: %v", err)
				return
			}
			_, _ = fmt.Fprint(w, strings.Join(notes, ","))
		})

		rec := httptest.NewRecorder()
		clean.ServeHTTP(rec, requestAsOwner(t, ownerA))

		assertNoLeak(t, rec, markerB)
		if rec.Body.Len() == 0 {
			t.Error("owner A saw nothing at all -- that is fail-shut, not isolation")
		}
	})

	// registeredRoutes finding nothing today is only meaningful if it would
	// find something when there is something. An empty result and a broken walk
	// look identical otherwise.
	t.Run("the route walk finds registered routes", func(t *testing.T) {
		r := chi.NewRouter()
		r.Get("/things", func(http.ResponseWriter, *http.Request) {})
		r.Patch("/things/{id}", func(http.ResponseWriter, *http.Request) {})

		got := walkRoutes(t, r)
		want := []route{
			{method: "GET", pattern: "/things"},
			{method: "PATCH", pattern: "/things/{id}"},
		}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("coverage guard reports an uncovered route", func(t *testing.T) {
		covered := route{method: "GET", pattern: "/covered"}
		uncovered := route{method: "GET", pattern: "/uncovered"}

		missing := routesWithoutCase(
			[]route{covered, uncovered},
			[]isolationCase{{route: covered}},
		)
		if len(missing) != 1 || missing[0] != uncovered {
			t.Errorf("got %v, want exactly %v", missing, uncovered)
		}
	})
}

// --- harness ---------------------------------------------------------------

// registeredRoutes lists what the generated code actually registers, so the
// coverage check cannot drift from the contract.
//
// It walks the same registration cmd/api serves -- HandlerFromMuxWithBaseURL
// via httpapi.NewRouter -- rather than a router built for the test, so a route
// that exists in production is a route this suite has to cover.
func registeredRoutes(t *testing.T) []route {
	t.Helper()

	r := chi.NewRouter()
	httpapi.HandlerFromMuxWithBaseURL(&httpapi.Server{}, r, "/api/v1")
	return walkRoutes(t, r)
}

// walkRoutes lists every method and pattern registered on a router.
func walkRoutes(t *testing.T, r chi.Router) []route {
	t.Helper()

	var routes []route
	err := chi.Walk(r, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routes = append(routes, route{method: method, pattern: pattern})
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}
	return routes
}

// routesWithoutCase returns the registered routes that no case covers.
func routesWithoutCase(registered []route, cases []isolationCase) []route {
	var missing []route
	for _, r := range registered {
		if !slices.ContainsFunc(cases, func(c isolationCase) bool { return c.route == r }) {
			missing = append(missing, r)
		}
	}
	return missing
}

// assertNoLeak fails when the response carries another owner's marker.
func assertNoLeak(t *testing.T, rec *httptest.ResponseRecorder, marker string) {
	t.Helper()
	if where := findLeak(rec, marker); where != "" {
		t.Errorf("owner B's row leaked in the response %s\nmarker: %s\nbody: %s",
			where, marker, rec.Body.String())
	}
}

// findLeak looks for the marker anywhere in the response -- body or headers --
// and names where it was found. Searching the raw bytes rather than a parsed
// shape means one check works for every route regardless of payload.
func findLeak(rec *httptest.ResponseRecorder, marker string) string {
	if strings.Contains(rec.Body.String(), marker) {
		return "body"
	}
	for name, values := range rec.Header() {
		for _, v := range values {
			if strings.Contains(v, marker) {
				return "header " + name
			}
		}
	}
	return ""
}

// requestAsOwner builds a request carrying an owner, standing in for the auth
// middleware until P1-011 issues real tokens.
func requestAsOwner(t *testing.T, id uuid.UUID) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	return req.WithContext(owner.NewContext(req.Context(), id))
}

// newServer builds the server the cases run against: the same router, the same
// middleware and the same handlers cmd/api serves, so a case cannot pass
// against a wiring that only exists in tests.
var newServer = func(t *testing.T) http.Handler {
	t.Helper()

	store, err := db.New(t.Context(), config.AppDatabaseURL())
	if err != nil {
		t.Fatalf("connect as app_user: %v\n\nIs PostgreSQL running and migrated?\n"+
			"  brew services start postgresql@18\n  make db-create && make migrate", err)
	}
	t.Cleanup(store.Close)

	signer, err := auth.NewSigner(isoSigningKey)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	// secureCookies false: httptest speaks plain HTTP.
	return httpapi.NewRouter(
		httpapi.NewServer(auth.NewService(store, signer), settings.New(store), false), signer)
}

// --- fixtures --------------------------------------------------------------

func openAppStore(ctx context.Context, t *testing.T) *db.Store {
	t.Helper()
	store, err := db.New(ctx, config.AppDatabaseURL())
	if err != nil {
		t.Fatalf("connect as app_user: %v\n\nIs PostgreSQL running and migrated?\n"+
			"  brew services start postgresql@18\n  make db-create && make migrate", err)
	}
	t.Cleanup(store.Close)
	return store
}

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

// seedRow inserts one row for an owner and returns a marker unique to it.
func seedRow(ctx context.Context, t *testing.T, store *db.Store, table string, ownerID uuid.UUID) string {
	t.Helper()

	marker := "marker-" + uuid.Must(uuid.NewV7()).String()
	err := store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			fmt.Sprintf(`INSERT INTO %s (owner_id, note) VALUES ($1, $2)`, table),
			ownerID, marker)
		return err
	})
	if err != nil {
		t.Fatalf("seed row for %s: %v", ownerID, err)
	}
	return marker
}

func allNotes(ctx context.Context, conn *pgx.Conn, table string) ([]string, error) {
	rows, err := conn.Query(ctx, fmt.Sprintf(`SELECT note FROM %s`, table))
	if err != nil {
		return nil, err
	}
	return collectNotes(rows)
}

func allNotesTx(ctx context.Context, tx pgx.Tx, table string) ([]string, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT note FROM %s`, table))
	if err != nil {
		return nil, err
	}
	return collectNotes(rows)
}

func collectNotes(rows pgx.Rows) ([]string, error) {
	defer rows.Close()
	var notes []string
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}

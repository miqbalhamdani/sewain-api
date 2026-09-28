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

// Isolation cases for the catalogue.  (S1-014 .. S1-017)
//
// The catalogue is the first thing in this system worth stealing. A leak here
// is not a session or an address list, it is a competitor's fleet, their prices
// and their deposits -- and nothing about a resource body makes a leak obvious
// on sight, which is why every case below marks the fixture with a name it
// would have to print.
//
// Nine cases, one per registered method-and-pattern. The coverage guard in
// isolation_test.go fails the moment a tenth route lands without one.
func init() {
	isolationCases = append(isolationCases,
		isolationCase{
			route: route{method: "GET", pattern: "/api/v1/resources"},
			seed:  seedCatalogOwner,
			request: func(t *testing.T, s seeded) *http.Request {
				return bearerRequest(t, http.MethodGet, "/api/v1/resources", s.accessToken)
			},
		},
		isolationCase{
			route: route{method: "POST", pattern: "/api/v1/resources"},
			seed:  seedCatalogOwner,
			request: func(t *testing.T, s seeded) *http.Request {
				return bodyRequest(t, http.MethodPost, "/api/v1/resources", s.accessToken,
					`{"name":"Iso Resource","base_price":350000}`)
			},
		},
		isolationCase{
			route: route{method: "GET", pattern: "/api/v1/resources/{id}"},
			seed:  seedCatalogOwner,
			request: func(t *testing.T, s seeded) *http.Request {
				// Owner B's resource id, reached with A's token. A 404 is the
				// only acceptable answer, and it must look exactly like the
				// answer for an id nobody ever issued (BR-001).
				return bearerRequest(t, http.MethodGet, "/api/v1/resources/"+s.resourceID, s.accessToken)
			},
		},
		isolationCase{
			route: route{method: "PATCH", pattern: "/api/v1/resources/{id}"},
			seed:  seedCatalogOwner,
			request: func(t *testing.T, s seeded) *http.Request {
				return bodyRequest(t, http.MethodPatch, "/api/v1/resources/"+s.resourceID,
					s.accessToken, `{"base_price":999000}`)
			},
		},
		isolationCase{
			route: route{method: "DELETE", pattern: "/api/v1/resources/{id}"},
			seed:  seedCatalogOwner,
			request: func(t *testing.T, s seeded) *http.Request {
				return bearerRequest(t, http.MethodDelete, "/api/v1/resources/"+s.resourceID, s.accessToken)
			},
		},
		isolationCase{
			route: route{method: "GET", pattern: "/api/v1/resources/{id}/units"},
			seed:  seedCatalogOwner,
			request: func(t *testing.T, s seeded) *http.Request {
				return bearerRequest(t, http.MethodGet,
					"/api/v1/resources/"+s.resourceID+"/units", s.accessToken)
			},
		},
		isolationCase{
			route: route{method: "POST", pattern: "/api/v1/resources/{id}/units"},
			seed:  seedCatalogOwner,
			request: func(t *testing.T, s seeded) *http.Request {
				// A fresh code each run: it is unique per owner, so a fixed one
				// would pass once and then collide with its own leftovers.
				code := "ISO " + uuid.Must(uuid.NewV7()).String()
				return bodyRequest(t, http.MethodPost,
					"/api/v1/resources/"+s.resourceID+"/units", s.accessToken,
					`{"code":"`+code+`"}`)
			},
		},
		isolationCase{
			route: route{method: "PATCH", pattern: "/api/v1/units/{id}"},
			seed:  seedCatalogOwner,
			request: func(t *testing.T, s seeded) *http.Request {
				return bodyRequest(t, http.MethodPatch, "/api/v1/units/"+s.unitID,
					s.accessToken, `{"status":"maintenance"}`)
			},
		},
		isolationCase{
			route: route{method: "DELETE", pattern: "/api/v1/units/{id}"},
			seed:  seedCatalogOwner,
			request: func(t *testing.T, s seeded) *http.Request {
				return bearerRequest(t, http.MethodDelete, "/api/v1/units/"+s.unitID, s.accessToken)
			},
		},
	)
}

// seedCatalogOwner gives a rental one resource with one unit, and marks the
// fixture by the resource's name.
//
// The name rather than the owner id, for the same reason seedSettingsOwner
// marks by slug: a resource body never carries an owner id, so a handler that
// returned somebody else's catalogue wholesale would sail past a marker the
// response could not have printed either way.
func seedCatalogOwner(ctx context.Context, t *testing.T, store *db.Store, ownerID uuid.UUID) seeded {
	t.Helper()

	s := seedSignedInOwner(ctx, t, store, ownerID)

	resourceID := uuid.Must(uuid.NewV7())
	unitID := uuid.Must(uuid.NewV7())
	name := "iso-resource-" + ownerID.String()
	code := "ISO-" + ownerID.String()

	ownerCtx := owner.NewContext(ctx, ownerID)
	if err := store.InOwnerTx(ownerCtx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO resources (id, owner_id, name, pricing_unit, base_price)
			 VALUES ($1, $2, $3, 'day', 350000)`,
			resourceID, ownerID, name); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO resource_units (id, owner_id, resource_id, code)
			 VALUES ($1, $2, $3, $4)`,
			unitID, ownerID, resourceID, code)
		return err
	}); err != nil {
		t.Fatalf("seed catalogue for owner %s: %v", ownerID, err)
	}

	t.Cleanup(func() {
		cleanup := context.WithoutCancel(ctx)
		_ = store.InOwnerTx(owner.NewContext(cleanup, ownerID), func(tx pgx.Tx) error {
			if _, err := tx.Exec(cleanup,
				`DELETE FROM resource_units WHERE owner_id = $1`, ownerID); err != nil {
				return err
			}
			_, err := tx.Exec(cleanup, `DELETE FROM resources WHERE owner_id = $1`, ownerID)
			return err
		})
	})

	s.marker = name
	s.resourceID = resourceID.String()
	s.unitID = unitID.String()
	return s
}

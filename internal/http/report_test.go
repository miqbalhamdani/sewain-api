package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
)

// M5 phase A over HTTP: the dashboard by role, the export job, the team
// guards. (S1-056, S1-057, S1-063, S1-067)

// seedUserIn adds a signed-in user of the given role to an existing rental.
func seedUserIn(ctx context.Context, t *testing.T, store *db.Store, ownerID uuid.UUID, role string) seeded {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	email := fmt.Sprintf("m5-%s@example.com", id)
	hash, err := auth.HashPassword(isoPassword)
	if err != nil {
		t.Fatal(err)
	}
	octx := owner.NewContext(ctx, ownerID)
	if err := store.InOwnerTx(octx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO users (id, owner_id, email, password_hash, name, role, status, email_verified_at)
		                        VALUES ($1, $2, $3, $4, 'M5 user', $5, 'active', now())`, id, ownerID, email, hash, role)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.WithoutCancel(ctx)
		_ = store.InOwnerTx(owner.NewContext(c, ownerID), func(tx pgx.Tx) error {
			_, err := tx.Exec(c, `DELETE FROM users WHERE id = $1`, id)
			return err
		})
	})
	signer, err := auth.NewSigner(isoSigningKey)
	if err != nil {
		t.Fatal(err)
	}
	session, err := auth.NewService(store, signer).Login(ctx, email, isoPassword)
	if err != nil {
		t.Fatal(err)
	}
	return seeded{userID: id.String(), accessToken: session.AccessToken}
}

// S1-067: an owner cannot demote or disable themselves, and the rental never
// loses its last active owner -- not even to a second owner whose token
// outlives their own disabling.
func TestTeamGuards(t *testing.T) {
	c := newBookingClient(t)
	self := c.s.userID

	expect(t, c.do(http.MethodPatch, "/api/v1/users/"+self, `{"role":"operator"}`), http.StatusUnprocessableEntity, "validation-failed")
	expect(t, c.do(http.MethodDelete, "/api/v1/users/"+self, ""), http.StatusUnprocessableEntity, "validation-failed")

	second := seedUserIn(t.Context(), t, c.store, c.ownerID, "owner")
	if w := c.do(http.MethodDelete, "/api/v1/users/"+second.userID, ""); w.Code != http.StatusNoContent {
		t.Fatalf("disabling another owner = %d, want 204; body=%s", w.Code, w.Body)
	}
	stale := c
	stale.token = second.accessToken
	expect(t, stale.do(http.MethodDelete, "/api/v1/users/"+self, ""), http.StatusUnprocessableEntity, "validation-failed")
}

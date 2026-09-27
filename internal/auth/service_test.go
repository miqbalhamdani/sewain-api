package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
)

const testPassword = "correct horse battery staple"

// TestRefreshReusedTokenRevokesEverything is P1-011's acceptance: a reused
// refresh token revokes the whole chain.
//
// It builds a real chain -- login, then two rotations -- and a second,
// independent chain from a separate login, then presents the first token again.
// API spec.md 2 asks for two things on reuse: the chain is revoked, and the user
// is signed out everywhere. The second chain is what tells them apart.
func TestRefreshReusedTokenRevokesEverything(t *testing.T) {
	ctx := t.Context()
	svc, store := newTestService(ctx, t)
	ownerID, userID, email := seedUser(ctx, t, store)

	// Chain one: login, then rotate twice.
	first, err := svc.Login(ctx, email, testPassword)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	second, err := svc.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatalf("first rotation: %v", err)
	}
	third, err := svc.Refresh(ctx, second.RefreshToken)
	if err != nil {
		t.Fatalf("second rotation: %v", err)
	}

	// Chain two: the same user signed in somewhere else.
	otherDevice, err := svc.Login(ctx, email, testPassword)
	if err != nil {
		t.Fatalf("second login: %v", err)
	}

	// Sanity before the interesting part: the newest token of each chain works.
	if countActiveTokens(ctx, t, store, ownerID, userID) != 2 {
		t.Fatalf("expected exactly two live tokens before the reuse, one per chain")
	}

	// The theft. `first` was rotated away two steps ago; the legitimate holder
	// would be carrying `third`. Anyone presenting `first` has an old copy.
	if _, err := svc.Refresh(ctx, first.RefreshToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("reusing a rotated token returned %v, want ErrUnauthenticated", err)
	}

	t.Run("the newest token of the reused chain is revoked", func(t *testing.T) {
		if _, err := svc.Refresh(ctx, third.RefreshToken); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("the live token of the compromised chain still works: %v", err)
		}
	})

	t.Run("the other device is signed out too", func(t *testing.T) {
		// This is the half that "revoke only this chain" would fail. The thief
		// holds a token from chain one; leaving chain two alive would mean the
		// user was told they were signed out everywhere while a second session
		// stayed open.
		if _, err := svc.Refresh(ctx, otherDevice.RefreshToken); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("a token from an unrelated session survived the reuse: %v", err)
		}
	})

	t.Run("no live token remains", func(t *testing.T) {
		if n := countActiveTokens(ctx, t, store, ownerID, userID); n != 0 {
			t.Errorf("%d token(s) still live after a reuse, want 0", n)
		}
	})

	t.Run("signing in again still works", func(t *testing.T) {
		// Revocation ends sessions; it does not lock the account. The user
		// whose token was stolen has to be able to get back in.
		if _, err := svc.Login(ctx, email, testPassword); err != nil {
			t.Errorf("login after a revocation: %v", err)
		}
	})
}

func TestLogin(t *testing.T) {
	ctx := t.Context()
	svc, store := newTestService(ctx, t)
	_, _, email := seedUser(ctx, t, store)

	t.Run("issues a session for the right credentials", func(t *testing.T) {
		s, err := svc.Login(ctx, email, testPassword)
		if err != nil {
			t.Fatalf("login: %v", err)
		}
		if s.AccessToken == "" || s.RefreshToken == "" {
			t.Error("session is missing a token")
		}
		if s.Owner.Name == "" {
			t.Errorf("owner not carried through: %+v", s.Owner)
		}
		// A business with no slug is the ordinary case and must log in, so this
		// fails the moment somebody puts NOT NULL back on the column (BR-005).
		if s.Owner.Slug != nil {
			t.Errorf("owner was seeded without a slug, got %q", *s.Owner.Slug)
		}
		// It is an empty list rather than absent because the contract marks
		// the field required.
		if s.User.Permissions == nil {
			t.Error("permissions is nil; the contract requires the field")
		}
	})

	// The other direction. Without this, wiring that always returned nil would
	// still pass the case above, and nothing else reads the slug until S1-051.
	t.Run("carries a slug through once the owner has one", func(t *testing.T) {
		ownerID, _, slugEmail := seedUser(ctx, t, store)
		if err := store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE owners SET slug = $2 WHERE id = $1`,
				ownerID, "rentalbudi-"+ownerID.String()[:8])
			return err
		}); err != nil {
			t.Fatalf("set slug: %v", err)
		}

		s, err := svc.Login(ctx, slugEmail, testPassword)
		if err != nil {
			t.Fatalf("login: %v", err)
		}
		if s.Owner.Slug == nil {
			t.Fatal("slug was set on the owner but arrived nil")
		}
		if want := "rentalbudi-" + ownerID.String()[:8]; *s.Owner.Slug != want {
			t.Errorf("slug = %q, want %q", *s.Owner.Slug, want)
		}
	})

	// A client must not be able to tell these apart. Any difference answers
	// "does this address have an account here" for free.
	t.Run("fails identically for a wrong password and an unknown email", func(t *testing.T) {
		_, wrongPassword := svc.Login(ctx, email, "not the password")
		_, unknownEmail := svc.Login(ctx, "nobody@example.com", testPassword)

		if !errors.Is(wrongPassword, ErrUnauthenticated) {
			t.Errorf("wrong password: got %v", wrongPassword)
		}
		if !errors.Is(unknownEmail, ErrUnauthenticated) {
			t.Errorf("unknown email: got %v", unknownEmail)
		}
		if wrongPassword.Error() != unknownEmail.Error() {
			t.Errorf("the two failures are distinguishable: %q vs %q",
				wrongPassword, unknownEmail)
		}
	})

	t.Run("refuses a disabled account", func(t *testing.T) {
		ownerID, userID, disabledEmail := seedUser(ctx, t, store)
		setUserStatus(ctx, t, store, ownerID, userID, "disabled")

		if _, err := svc.Login(ctx, disabledEmail, testPassword); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("a disabled user signed in: %v", err)
		}
	})
}

// TestDisabledAccountCannotRefresh is S1-010's "sesi mati <= 15 menit".
//
// Login has always refused a disabled account. Refresh did not: it read the
// user row for the role and never looked at the status, so a revoked account
// went on minting access tokens every fifteen minutes for as long as its
// refresh token lived -- thirty days. The cap BR-004 promises only held for
// accounts nobody had actually revoked.
func TestDisabledAccountCannotRefresh(t *testing.T) {
	ctx := t.Context()
	svc, store := newTestService(ctx, t)

	ownerID, userID, email := seedUser(ctx, t, store)

	session, err := svc.Login(ctx, email, testPassword)
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	// Refresh works while the account is active, so the assertion below is
	// about the status and not about some unrelated breakage.
	if _, err := svc.Refresh(ctx, session.RefreshToken); err != nil {
		t.Fatalf("refresh while active: %v", err)
	}

	session, err = svc.Login(ctx, email, testPassword)
	if err != nil {
		t.Fatalf("second login: %v", err)
	}

	if err := store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, userID)
		return err
	}); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	if _, err := svc.Refresh(ctx, session.RefreshToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("refresh after disable = %v, want ErrUnauthenticated", err)
	}
}

func TestRefreshAndLogout(t *testing.T) {
	ctx := t.Context()
	svc, store := newTestService(ctx, t)
	ownerID, userID, email := seedUser(ctx, t, store)

	t.Run("rotation replaces the token it was given", func(t *testing.T) {
		first, err := svc.Login(ctx, email, testPassword)
		if err != nil {
			t.Fatalf("login: %v", err)
		}
		second, err := svc.Refresh(ctx, first.RefreshToken)
		if err != nil {
			t.Fatalf("rotate: %v", err)
		}
		if second.RefreshToken == first.RefreshToken {
			t.Error("rotation returned the same token")
		}
		if second.AccessToken == "" {
			t.Error("rotation returned no access token")
		}
	})

	t.Run("an unknown token is refused without revoking anything", func(t *testing.T) {
		live, err := svc.Login(ctx, email, testPassword)
		if err != nil {
			t.Fatalf("login: %v", err)
		}
		before := countActiveTokens(ctx, t, store, ownerID, userID)

		if _, err := svc.Refresh(ctx, "a-token-that-was-never-issued"); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("got %v, want ErrUnauthenticated", err)
		}
		// A token that was never issued is a typo or a probe, not evidence of
		// theft. Treating it as theft would let anyone sign out any user by
		// guessing.
		if after := countActiveTokens(ctx, t, store, ownerID, userID); after != before {
			t.Errorf("live tokens went from %d to %d; an unknown token must revoke nothing", before, after)
		}
		if _, err := svc.Refresh(ctx, live.RefreshToken); err != nil {
			t.Errorf("the live token stopped working: %v", err)
		}
	})

	t.Run("logout revokes only the presented token", func(t *testing.T) {
		one, err := svc.Login(ctx, email, testPassword)
		if err != nil {
			t.Fatalf("login: %v", err)
		}
		two, err := svc.Login(ctx, email, testPassword)
		if err != nil {
			t.Fatalf("login: %v", err)
		}

		if err := svc.Logout(ctx, one.RefreshToken); err != nil {
			t.Fatalf("logout: %v", err)
		}
		if _, err := svc.Refresh(ctx, one.RefreshToken); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("the signed-out token still rotates: %v", err)
		}
		// Signing out of one device must not sign out the others.
		if _, err := svc.Refresh(ctx, two.RefreshToken); err != nil {
			t.Errorf("logout on one session ended another: %v", err)
		}
	})

	t.Run("logout is silent on a token it does not know", func(t *testing.T) {
		if err := svc.Logout(ctx, "never-issued"); err != nil {
			t.Errorf("got %v, want nil -- there is nothing useful to report", err)
		}
		if err := svc.Logout(ctx, ""); err != nil {
			t.Errorf("got %v, want nil", err)
		}
	})
}

// --- fixtures --------------------------------------------------------------

func newTestService(ctx context.Context, t *testing.T) (*Service, *db.Store) {
	t.Helper()

	store, err := db.New(ctx, config.AppDatabaseURL())
	if err != nil {
		t.Fatalf("connect as app_user: %v\n\nIs PostgreSQL running and migrated?\n"+
			"  brew services start postgresql@18\n  make db-create && make migrate", err)
	}
	t.Cleanup(store.Close)

	signer, err := NewSigner(testSecret)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return NewService(store, signer), store
}

// seedUser creates an owner and one active user with testPassword, and removes
// them when the test ends.
func seedUser(ctx context.Context, t *testing.T, store *db.Store) (ownerID, userID uuid.UUID, email string) {
	t.Helper()

	ownerID = uuid.Must(uuid.NewV7())
	userID = uuid.Must(uuid.NewV7())
	email = fmt.Sprintf("user-%s@example.com", userID)

	hash, err := HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	// owners has no RLS by design, so this needs no owner context.
	if err := store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			// No slug: that is the initial state of every business, not an edge
			// case -- it is not asked for at registration and is filled in later
			// from PATCH /settings (BR-005, BR-025).
			`INSERT INTO owners (id, name) VALUES ($1, $2)`,
			ownerID, "Test owner"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO users (id, owner_id, email, password_hash, name, role, status)
			 VALUES ($1, $2, $3, $4, 'Test user', 'operator', 'active')`,
			userID, ownerID, email, hash)
		return err
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	t.Cleanup(func() {
		cleanup := context.WithoutCancel(ctx)
		_ = store.InOwnerTx(owner.NewContext(cleanup, ownerID), func(tx pgx.Tx) error {
			// refresh_tokens cascades from users.
			if _, err := tx.Exec(cleanup, `DELETE FROM users WHERE id = $1`, userID); err != nil {
				return err
			}
			_, err := tx.Exec(cleanup, `DELETE FROM owners WHERE id = $1`, ownerID)
			return err
		})
	})
	return ownerID, userID, email
}

func setUserStatus(ctx context.Context, t *testing.T, store *db.Store, ownerID, userID uuid.UUID, status string) {
	t.Helper()
	if err := store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE users SET status = $2 WHERE id = $1`, userID, status)
		return err
	}); err != nil {
		t.Fatalf("set status: %v", err)
	}
}

func countActiveTokens(ctx context.Context, t *testing.T, store *db.Store, ownerID, userID uuid.UUID) int {
	t.Helper()

	var n int
	if err := store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`,
			userID).Scan(&n)
	}); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	return n
}

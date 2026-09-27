package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// Registration.  (S1-082, BR-005)
//
// This is the entry point of the whole system. Until it existed, `login` had
// no row to verify against and every dependency chain in the project bottomed
// out at "assume the owners row is already there".

// businessTypes is the six market presets. The schema CHECK holds the same
// list, and this one exists so a bad value is a 422 with a useful message
// rather than a constraint violation translated after the fact.
//
// Six here, two on the registration screen: boarding_house and apartment are
// phase 2, venue phase 3, clinic phase 4 (BR-017). The schema knowing a value
// and the product offering it are different things.
var businessTypes = map[string]bool{
	"vehicle_rental": true, "equipment_rental": true,
	"boarding_house": true, "apartment": true,
	"venue": true, "clinic": true,
}

// Register creates a business and its owner in one transaction.
//
// Both rows or neither: a business without an owner is a row nobody can ever
// log into, and it would sit there holding the email address hostage against
// the retry.
func (s *Service) Register(ctx context.Context, email, password, businessName, businessType string) (Session, error) {
	if email == "" || businessName == "" {
		return Session{}, apperrors.ValidationFailed("email and business_name are required.")
	}
	if len(password) < 8 {
		return Session{}, apperrors.ValidationFailed("password is at least 8 characters.")
	}
	if !businessTypes[businessType] {
		return Session{}, apperrors.ValidationFailed(
			"business_type is one of vehicle_rental, equipment_rental, boarding_house, " +
				"apartment, venue, clinic.").WithFields(apperrors.Field{Name: "business_type"})
	}

	hash, err := HashPassword(password)
	if err != nil {
		return Session{}, fmt.Errorf("hash password: %w", err)
	}

	ownerID, err := uuid.NewV7()
	if err != nil {
		return Session{}, fmt.Errorf("new owner id: %w", err)
	}
	userID, err := uuid.NewV7()
	if err != nil {
		return Session{}, fmt.Errorf("new user id: %w", err)
	}

	// InOwnerTx wants an owner in the context, and here the owner is the thing
	// being created. That works because the id is minted in Go rather than by
	// the database, so it is known before the INSERT -- and setting
	// app.owner_id to it is what lets the RLS policy on `users` accept the row
	// written a statement later.
	ownerCtx := owner.NewContext(ctx, ownerID)

	err = s.store.InOwnerTx(ownerCtx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if err := q.CreateOwner(ctx, sqlcgen.CreateOwnerParams{
			ID: ownerID, Name: businessName, BusinessType: businessType,
		}); err != nil {
			return err
		}
		return q.CreateOwnerUser(ctx, sqlcgen.CreateOwnerUserParams{
			ID: userID, OwnerID: ownerID, Email: email,
			PasswordHash: &hash,
			// The person's own name is not asked for at registration -- one
			// less field, and the business name is what the UI shows anyway.
			Name: businessName,
		})
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.ConstraintName {
			case "users_email_key":
				return Session{}, apperrors.EmailTaken().WithCause(err)
			case "owners_business_type_valid":
				return Session{}, apperrors.ValidationFailed(
					"business_type is not a known preset.").WithCause(err)
			}
		}
		return Session{}, fmt.Errorf("register: %w", err)
	}

	// The session is issued even though the account is not verified yet, and
	// that is the point: it is what lets the new owner call resend without
	// logging in again. They land on the verification wall, not the dashboard
	// (BR-006).
	return s.issue(ownerCtx, ownerID, userID, nil)
}

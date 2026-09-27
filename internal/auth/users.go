package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// Account administration.  (S1-010)
//
// It lives in this package because this package already owns the users table --
// Login, Refresh and Me all read it, and the role matrix in roles.go is the
// authority on what a row may do. Splitting the writes into their own package
// would put two owners on one table.
//
// Every query here runs inside InOwnerTx, so RLS has already scoped users to
// one rental. That is what makes an id from another rental return zero rows,
// and zero rows is a 404 -- there is no ownership check in Go and none wanted,
// because a check that can be forgotten is a check that will be (BR-001).

// ListUsers returns the accounts in the caller's rental.
func (s *Service) ListUsers(ctx context.Context) ([]sqlcgen.ListUsersRow, error) {
	var rows []sqlcgen.ListUsersRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlcgen.New(tx).ListUsers(ctx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return rows, nil
}

// InviteUser creates an account that has no password yet.
//
// The row is what the invitee later accepts; sending the invitation is S1-084's
// job, not this one. Until then the account exists, is visible to the owner, and
// cannot log in -- password_hash is NULL and Login refuses it.
func (s *Service) InviteUser(ctx context.Context, ownerID uuid.UUID, email, name, role string) (sqlcgen.InviteUserRow, error) {
	if role != RoleOwner && role != RoleOperator {
		return sqlcgen.InviteUserRow{}, apperrors.ValidationFailed(
			"role is either owner or operator.")
	}

	id, err := uuid.NewV7()
	if err != nil {
		return sqlcgen.InviteUserRow{}, fmt.Errorf("new user id: %w", err)
	}

	var row sqlcgen.InviteUserRow
	err = s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).InviteUser(ctx, sqlcgen.InviteUserParams{
			ID: id, OwnerID: ownerID, Email: email, Name: name, Role: role,
		})
		return err
	})
	if err != nil {
		// The address is unique across the whole system, not per rental, and
		// that is what buys login its missing business parameter (BR-004).
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "users_email_key" {
			return sqlcgen.InviteUserRow{}, apperrors.EmailTaken().WithCause(err)
		}
		return sqlcgen.InviteUserRow{}, fmt.Errorf("invite user: %w", err)
	}
	return row, nil
}

// UpdateUser changes a role or a status, and nothing else.
//
// email and owner_id are absent from the query on purpose: one user belongs to
// exactly one rental, and the address is what points at it when they log in.
func (s *Service) UpdateUser(ctx context.Context, id uuid.UUID, role, status *string) (sqlcgen.UpdateUserRow, error) {
	if role == nil && status == nil {
		return sqlcgen.UpdateUserRow{}, apperrors.ValidationFailed(
			"Send at least one of role or status.")
	}
	if role != nil && *role != RoleOwner && *role != RoleOperator {
		return sqlcgen.UpdateUserRow{}, apperrors.ValidationFailed(
			"role is either owner or operator.")
	}
	// 'invited' is the server's to set, once, when the account is created.
	if status != nil && *status != "active" && *status != "disabled" {
		return sqlcgen.UpdateUserRow{}, apperrors.ValidationFailed(
			"status is either active or disabled.")
	}

	var row sqlcgen.UpdateUserRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).UpdateUser(ctx, sqlcgen.UpdateUserParams{
			ID: id, Role: role, Status: status,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.UpdateUserRow{}, notFoundUser()
	}
	if err != nil {
		return sqlcgen.UpdateUserRow{}, fmt.Errorf("update user: %w", err)
	}
	return row, nil
}

// DisableUser flips status to disabled. It never deletes the row.
//
// created_by columns elsewhere have to stay explicable, and history pointing at
// an account nobody can name is history nobody can read.
//
// The session dies within the access token's 15 minutes: Refresh re-reads the
// row and refuses anything that is not active, so nothing outlives that bound.
func (s *Service) DisableUser(ctx context.Context, id uuid.UUID) error {
	return s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)

		affected, err := q.DisableUser(ctx, id)
		if err != nil {
			return fmt.Errorf("disable user: %w", err)
		}
		if affected > 0 {
			return nil
		}

		// Zero rows is ambiguous: already disabled, or not in this rental at
		// all. Only the second is a 404, and RLS is what makes the difference
		// invisible to the caller either way.
		exists, err := q.UserExists(ctx, id)
		if err != nil {
			return fmt.Errorf("disable user: %w", err)
		}
		if !exists {
			return notFoundUser()
		}
		return nil // already disabled; deleting twice is still deleted
	})
}

func notFoundUser() error {
	return apperrors.NotFound("No such user in this business.")
}

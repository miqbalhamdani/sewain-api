package auth

import (
	"context"
	"errors"
	"fmt"

	"time"

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

// Account is one user as this package talks about them.
//
// Not the sqlc row type: that one is regenerated from the migrations, so
// letting it out would make a column rename ripple into the HTTP layer. The
// three queries below return the same six columns, so they share one type --
// and password_hash is not among them, here or in the queries.
type Account struct {
	ID          uuid.UUID
	Email       string
	Name        string
	Role        string
	Status      string
	LastLoginAt *time.Time
}

// ListUsers returns the accounts in the caller's rental.
func (s *Service) ListUsers(ctx context.Context) ([]Account, error) {
	var rows []sqlcgen.ListUsersRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlcgen.New(tx).ListUsers(ctx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}

	// An empty slice, never nil: the contract types this as a list, and a
	// client that has to handle both shapes will handle one of them wrong.
	accounts := make([]Account, 0, len(rows))
	for _, row := range rows {
		accounts = append(accounts,
			accountOf(row.ID, row.Email, row.Name, row.Role, row.Status, row.LastLoginAt))
	}
	return accounts, nil
}

// InviteUser creates an account that has no password yet.
//
// The row is what the invitee later accepts; sending the invitation is S1-084's
// job, not this one. Until then the account exists, is visible to the owner, and
// cannot log in -- password_hash is NULL and Login refuses it.
func (s *Service) InviteUser(ctx context.Context, ownerID uuid.UUID, email, name, role string) (Account, error) {
	if role != RoleOwner && role != RoleOperator {
		return Account{}, apperrors.ValidationFailed(
			"role is either owner or operator.")
	}

	id, err := uuid.NewV7()
	if err != nil {
		return Account{}, fmt.Errorf("new user id: %w", err)
	}

	var row sqlcgen.InviteUserRow
	// The business name goes into the invitation mail so the invitee knows
	// which rental is asking. Read in the same transaction rather than passed
	// in by the handler: a client-supplied name would let anyone send a
	// convincing invitation wearing somebody else's business.
	var businessName string
	err = s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var err error
		if businessName, err = q.GetOwnerName(ctx, ownerID); err != nil {
			return err
		}
		row, err = q.InviteUser(ctx, sqlcgen.InviteUserParams{
			ID: id, OwnerID: ownerID, Email: email, Name: name, Role: role,
		})
		return err
	})
	if err != nil {
		// The address is unique across the whole system, not per rental, and
		// that is what buys login its missing business parameter (BR-004).
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "users_email_key" {
			return Account{}, apperrors.EmailTaken().WithCause(err)
		}
		return Account{}, fmt.Errorf("invite user: %w", err)
	}
	account := accountOf(row.ID, row.Email, row.Name, row.Role, row.Status, row.LastLoginAt)

	// The invitation link is the only way into this account, and it is also
	// what will mark the address verified when it is accepted -- so failing to
	// send it leaves a row nobody can ever use (BR-004, BR-006).
	//
	// Sent after the transaction commits, not inside it: a mail that goes out
	// for a row that then rolls back is a link to nothing, and unsending is
	// not a thing.
	if err := s.SendInvitation(ctx, ownerID, row.ID, row.Email, row.Name, businessName); err != nil {
		return account, fmt.Errorf("invitation sent nowhere: %w", err)
	}
	return account, nil
}

// UpdateUser changes a role or a status, and nothing else.
//
// email and owner_id are absent from the query on purpose: one user belongs to
// exactly one rental, and the address is what points at it when they log in.
func (s *Service) UpdateUser(ctx context.Context, id uuid.UUID, role, status *string) (Account, error) {
	if role == nil && status == nil {
		return Account{}, apperrors.ValidationFailed(
			"Send at least one of role or status.")
	}
	if role != nil && *role != RoleOwner && *role != RoleOperator {
		return Account{}, apperrors.ValidationFailed(
			"role is either owner or operator.")
	}
	// 'invited' is the server's to set, once, when the account is created.
	if status != nil && *status != "active" && *status != "disabled" {
		return Account{}, apperrors.ValidationFailed(
			"status is either active or disabled.")
	}

	var row sqlcgen.UpdateUserRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		losesOwner := (role != nil && *role == RoleOperator) || (status != nil && *status == "disabled")
		if err := guardOwners(ctx, sqlcgen.New(tx), id, losesOwner); err != nil {
			return err
		}
		var err error
		row, err = sqlcgen.New(tx).UpdateUser(ctx, sqlcgen.UpdateUserParams{
			ID: id, Role: role, Status: status,
		})
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, notFoundUser()
	}
	if err != nil {
		return Account{}, fmt.Errorf("update user: %w", err)
	}
	return accountOf(row.ID, row.Email, row.Name, row.Role, row.Status, row.LastLoginAt), nil
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
		if err := guardOwners(ctx, q, id, true); err != nil {
			return err
		}

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

// guardOwners refuses a change that takes owner access from the caller
// themselves, or from the rental's last active owner -- either locks the
// business out of its own settings for good (S1-067).
func guardOwners(ctx context.Context, q *sqlcgen.Queries, id uuid.UUID, losesOwner bool) error {
	if !losesOwner {
		return nil
	}
	if self, _ := UserFromContext(ctx); self == id {
		return apperrors.ValidationFailed("You cannot disable or demote your own account.")
	}
	owners, err := q.LockActiveOwners(ctx)
	if err != nil {
		return fmt.Errorf("lock owners: %w", err)
	}
	if len(owners) == 1 && owners[0] == id {
		return apperrors.ValidationFailed("A business needs at least one active owner.")
	}
	return nil
}

func notFoundUser() error {
	return apperrors.NotFound("No such user in this business.")
}

// accountOf is the one place a database row becomes an Account. The three
// queries return the same six columns, so they share one converter.
func accountOf(id uuid.UUID, email, name, role, status string, lastLoginAt *time.Time) Account {
	return Account{
		ID: id, Email: email, Name: name,
		Role: role, Status: status, LastLoginAt: lastLoginAt,
	}
}

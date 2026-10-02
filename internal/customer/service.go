// Package customer holds the renters of one rental.  (S1-020, S1-021)
//
// Two things here are not plain CRUD. The identity number is encrypted before
// it reaches the database and never comes back out whole (BR-085). And blocking
// a renter is a separate act with its own permission and its own audit row,
// because an operator may edit a customer but may not block one (BR-028).
package customer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

type Service struct {
	store *db.Store
	box   *identityBox
}

// New takes the 32-byte key from config.IdentityKey.
func New(store *db.Store, identityKey []byte) (*Service, error) {
	box, err := newIdentityBox(identityKey)
	if err != nil {
		return nil, err
	}
	return &Service{store: store, box: box}, nil
}

// Customer is what any backoffice role may read. The identity number is not
// in it -- only its last four digits.
type Customer struct {
	ID              uuid.UUID
	Name            string
	Phone           string
	IDType          *string
	IDNumberLast4   *string
	IsBlacklisted   bool
	BlacklistReason *string
	CreatedAt       time.Time
}

// Input is what create and update accept. On update every nil field is left
// alone; on create Name and Phone are required by the handler.
type Input struct {
	Name     *string
	Phone    *string
	IDType   *string
	IDNumber *string
}

func notFound() error { return apperrors.NotFound("No such customer in this business.") }

func (s *Service) Create(ctx context.Context, userID uuid.UUID, in Input) (Customer, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return Customer{}, db.ErrNoOwnerContext
	}
	enc, last4, err := s.sealNumber(in)
	if err != nil {
		return Customer{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Customer{}, fmt.Errorf("new customer id: %w", err)
	}

	var row sqlcgen.GetCustomerRow
	err = s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if err := q.CreateCustomer(ctx, sqlcgen.CreateCustomerParams{
			ID: id, OwnerID: ownerID, CreatedBy: &userID,
			Name: deref(in.Name), Phone: deref(in.Phone), IDType: in.IDType,
			IDNumberEnc: enc, IDNumberLast4: last4,
		}); err != nil {
			return err
		}
		row, err = q.GetCustomer(ctx, id)
		return err
	})
	if err != nil {
		return Customer{}, translate(err, "create customer")
	}
	return customerOf(row), nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Customer, error) {
	var row sqlcgen.GetCustomerRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).GetCustomer(ctx, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Customer{}, notFound()
	}
	if err != nil {
		return Customer{}, fmt.Errorf("get customer: %w", err)
	}
	return customerOf(row), nil
}

// List returns one page, newest first, and the cursor for the next page (nil
// on the last one).
// blacklisted nil means both.
func (s *Service) List(ctx context.Context, q *string, blacklisted *bool, cursor *uuid.UUID, limit int) ([]Customer, *uuid.UUID, error) {
	var rows []sqlcgen.ListCustomersRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlcgen.New(tx).ListCustomers(ctx, sqlcgen.ListCustomersParams{
			Q: q, Blacklisted: blacklisted, Cursor: cursor, Lim: int32(limit + 1), //nolint:gosec // bounded by the schema's maximum
		})
		return err
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list customers: %w", err)
	}
	var next *uuid.UUID
	if len(rows) > limit {
		rows = rows[:limit]
		next = &rows[limit-1].ID
	}
	out := make([]Customer, 0, len(rows))
	for _, r := range rows {
		out = append(out, customerOf(sqlcgen.GetCustomerRow(r)))
	}
	return out, next, nil
}

func (s *Service) Update(ctx context.Context, id uuid.UUID, in Input) (Customer, error) {
	enc, last4, err := s.sealNumber(in)
	if err != nil {
		return Customer{}, err
	}
	var row sqlcgen.GetCustomerRow
	var found int64
	err = s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var err error
		if found, err = q.UpdateCustomer(ctx, sqlcgen.UpdateCustomerParams{
			ID: id, Name: in.Name, Phone: in.Phone, IDType: in.IDType,
			IDNumberEnc: enc, IDNumberLast4: last4,
		}); err != nil || found == 0 {
			return err
		}
		row, err = q.GetCustomer(ctx, id)
		return err
	})
	if err != nil {
		return Customer{}, translate(err, "update customer")
	}
	if found == 0 {
		return Customer{}, notFound()
	}
	return customerOf(row), nil
}

// SetBlacklist blocks (reason non-nil) or unblocks (reason nil) a customer, and
// writes the audit row in the same transaction -- a block nobody can attribute
// is a block nobody can explain to the renter it turned away (BR-028).
func (s *Service) SetBlacklist(ctx context.Context, actorID, id uuid.UUID, reason *string) (Customer, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return Customer{}, db.ErrNoOwnerContext
	}
	auditID, err := uuid.NewV7()
	if err != nil {
		return Customer{}, fmt.Errorf("new audit id: %w", err)
	}
	action, metadata := "customer.unblacklisted", []byte(`{}`)
	if reason != nil {
		action = "customer.blacklisted"
		if metadata, err = json.Marshal(map[string]string{"reason": *reason}); err != nil {
			return Customer{}, err
		}
	}

	var row sqlcgen.GetCustomerRow
	var found int64
	err = s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var err error
		if found, err = q.SetCustomerBlacklist(ctx, sqlcgen.SetCustomerBlacklistParams{
			ID: id, IsBlacklisted: reason != nil, BlacklistReason: reason,
		}); err != nil || found == 0 {
			return err
		}
		if err := q.InsertAuditLog(ctx, sqlcgen.InsertAuditLogParams{
			ID: auditID, OwnerID: ownerID, ActorUserID: actorID,
			Action: action, Entity: "customer", EntityID: id, Metadata: metadata,
		}); err != nil {
			return err
		}
		row, err = q.GetCustomer(ctx, id)
		return err
	})
	if err != nil {
		return Customer{}, translate(err, "set blacklist")
	}
	if found == 0 {
		return Customer{}, notFound()
	}
	return customerOf(row), nil
}

// sealNumber encrypts the identity number if one was given. A number without
// its type is refused: "7890" alone does not say which card to check.
func (s *Service) sealNumber(in Input) ([]byte, *string, error) {
	if in.IDNumber == nil {
		return nil, nil, nil
	}
	if in.IDType == nil {
		return nil, nil, apperrors.ValidationFailed("id_type is required when id_number is sent.").
			WithFields(apperrors.Field{Name: "id_type"})
	}
	enc, err := s.box.seal(*in.IDNumber)
	if err != nil {
		return nil, nil, err
	}
	last4 := lastFour(*in.IDNumber)
	return enc, &last4, nil
}

func translate(err error, verb string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.ConstraintName {
		case "customers_id_type_valid":
			return apperrors.ValidationFailed("That identity type is not one this system has.").
				WithFields(apperrors.Field{Name: "id_type"}).WithCause(err)
		case "customers_blacklist_has_reason":
			// Unreachable through the handler, which requires a reason.
			return apperrors.ValidationFailed("A block needs a reason.").
				WithFields(apperrors.Field{Name: "reason"}).WithCause(err)
		}
	}
	return fmt.Errorf("%s: %w", verb, err)
}

func customerOf(r sqlcgen.GetCustomerRow) Customer {
	return Customer{
		ID: r.ID, Name: r.Name, Phone: r.Phone, IDType: r.IDType,
		IDNumberLast4: r.IDNumberLast4, IsBlacklisted: r.IsBlacklisted,
		BlacklistReason: r.BlacklistReason, CreatedAt: r.CreatedAt,
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

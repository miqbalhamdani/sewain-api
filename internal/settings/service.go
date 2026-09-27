// Package settings holds the owner knobs.  (S1-009)
//
// Every field it manages exists because a business rule forbids hard-coding it
// -- BR-024, BR-025, BR-027, BR-038, BR-057, BR-070 -- and this package is the
// only writer. A consumer that keeps its own default has put back the thing
// those six rules were written to prevent.
//
// It is a package rather than a method on internal/owner because internal/db
// imports internal/owner for the request context, so owner cannot import db
// back. Domain packages are assembled in internal/http, per CLAUDE.md.
package settings

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

type Service struct {
	store *db.Store
}

func New(store *db.Store) *Service { return &Service{store: store} }

// Get reads this rental's knobs. The id comes from the owner context, never
// from the request -- owners has no RLS to fall back on (it IS the tenant), so
// the WHERE clause is the whole of the isolation here.
func (s *Service) Get(ctx context.Context) (sqlcgen.GetSettingsRow, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return sqlcgen.GetSettingsRow{}, db.ErrNoOwnerContext
	}

	var row sqlcgen.GetSettingsRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).GetSettings(ctx, ownerID)
		return err
	})
	if err != nil {
		return sqlcgen.GetSettingsRow{}, fmt.Errorf("get settings: %w", err)
	}
	return row, nil
}

// Update applies the knobs the caller actually sent.
//
// Validation is the database's, not this function's. The CHECK constraints in
// 000004 already refuse a bad prefix, a zero payment_due_hours, and a negative
// tolerance; re-stating them here would give two places to disagree. What this
// does is translate the refusal into the error code the contract names.
func (s *Service) Update(ctx context.Context, arg sqlcgen.UpdateSettingsParams) (sqlcgen.UpdateSettingsRow, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return sqlcgen.UpdateSettingsRow{}, db.ErrNoOwnerContext
	}
	arg.ID = ownerID

	var row sqlcgen.UpdateSettingsRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).UpdateSettings(ctx, arg)
		return err
	})
	if err != nil {
		return sqlcgen.UpdateSettingsRow{}, translate(err)
	}
	return row, nil
}

// translate maps a constraint violation to the code in 04-api-spec.md section 2.
//
// Constraint names, not string matching on the message: the message is
// PostgreSQL's to change, the name is ours.
func translate(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("update settings: %w", err)
	}

	switch pgErr.ConstraintName {
	case "owners_slug_unique":
		return apperrors.SlugTaken().WithCause(err)
	case "owners_slug_format":
		return apperrors.SlugInvalid(
			"A subdomain is 3-63 characters of lowercase letters, digits and hyphens, " +
				"and cannot start or end with a hyphen.").WithCause(err)
	case "owners_slug_not_punycode":
		return apperrors.SlugInvalid(
			"A subdomain cannot have a hyphen in its third and fourth position.").WithCause(err)
	case "owners_slug_not_reserved":
		return apperrors.SlugInvalid(
			"That subdomain is reserved.").WithCause(err)
	case "owners_booking_code_prefix_format":
		return apperrors.ValidationFailed(
			"booking_code_prefix is 2-6 uppercase letters or digits.").WithCause(err)
	case "owners_payment_due_hours_positive":
		return apperrors.ValidationFailed(
			"payment_due_hours is at least 1: an invoice due on the instant it is issued " +
				"is past due on the instant it is issued.").WithCause(err)
	case "owners_no_show_tolerance_non_negative":
		return apperrors.ValidationFailed(
			"no_show_tolerance_hours cannot be negative. Zero is allowed and means the " +
				"unit is freed as soon as the booking starts.").WithCause(err)
	}
	return fmt.Errorf("update settings: %w", err)
}

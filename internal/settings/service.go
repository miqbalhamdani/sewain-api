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

// Knobs is one rental's settings as this package talks about them.
//
// Not the sqlc row type: that one is regenerated from the migrations, so
// letting it out of this package would make a column rename ripple into the
// HTTP layer. The conversion lives here, once.
type Knobs struct {
	Slug                       *string
	BookingCodePrefix          string
	RequirePaymentBeforePickup bool
	DraftExpiryHours           int
	PaymentDueHours            int
	NoShowToleranceHours       int
	NotifyPickupReminder       bool
	NotifyReturnReminder       bool
	NotifyOverdueReminder      bool

	// Profil usaha (BR-096). Bukan knob melainkan identitas, dan ketiganya
	// nullable karena itu keadaan awal tiap usaha baru -- pendaftaran cuma
	// menanyakan empat hal (BR-005). Halaman publik tidak hidup sebelum Slug,
	// WhatsApp, dan Address ketiganya terisi.
	WhatsApp       *string
	Address        *string
	OperatingHours *string

	// Rekening transfer yang ditampilkan portal penyewa (S1-062), dan origin
	// situs pemilik yang boleh memanggil api.sewain.id dari browser (BR-031).
	BankName          *string
	BankAccountNumber *string
	BankAccountHolder *string
	AllowedOrigins    []string
}

// Patch is what a caller is changing. A nil field means the key was absent and
// the stored value stands -- absent and null are not the same thing, and the
// query COALESCEs on exactly this distinction.
type Patch struct {
	Slug                       *string
	BookingCodePrefix          *string
	RequirePaymentBeforePickup *bool
	DraftExpiryHours           *int
	PaymentDueHours            *int
	NoShowToleranceHours       *int
	NotifyPickupReminder       *bool
	NotifyReturnReminder       *bool
	NotifyOverdueReminder      *bool

	WhatsApp       *string
	Address        *string
	OperatingHours *string

	BankName          *string
	BankAccountNumber *string
	BankAccountHolder *string
	// nil = absent; an empty slice clears the list.
	AllowedOrigins []string
}

// Get reads this rental's knobs. The id comes from the owner context, never
// from the request -- owners has no RLS to fall back on (it IS the tenant), so
// the WHERE clause is the whole of the isolation here.
func (s *Service) Get(ctx context.Context) (Knobs, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return Knobs{}, db.ErrNoOwnerContext
	}

	var row sqlcgen.GetSettingsRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).GetSettings(ctx, ownerID)
		return err
	})
	if err != nil {
		return Knobs{}, fmt.Errorf("get settings: %w", err)
	}
	return Knobs(knobsOf(row)), nil
}

// Update applies the knobs the caller actually sent.
//
// Validation is the database's, not this function's. The CHECK constraints in
// 000004 already refuse a bad prefix, a zero payment_due_hours, and a negative
// tolerance; re-stating them here would give two places to disagree. What this
// does is translate the refusal into the error code the contract names.
func (s *Service) Update(ctx context.Context, p Patch) (Knobs, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return Knobs{}, db.ErrNoOwnerContext
	}

	var row sqlcgen.UpdateSettingsRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).UpdateSettings(ctx, sqlcgen.UpdateSettingsParams{
			ID:                         ownerID,
			Slug:                       p.Slug,
			BookingCodePrefix:          p.BookingCodePrefix,
			RequirePaymentBeforePickup: p.RequirePaymentBeforePickup,
			DraftExpiryHours:           int32Ptr(p.DraftExpiryHours),
			PaymentDueHours:            int32Ptr(p.PaymentDueHours),
			NoShowToleranceHours:       int32Ptr(p.NoShowToleranceHours),
			NotifyPickupReminder:       p.NotifyPickupReminder,
			NotifyReturnReminder:       p.NotifyReturnReminder,
			NotifyOverdueReminder:      p.NotifyOverdueReminder,
			Whatsapp:                   p.WhatsApp,
			Address:                    p.Address,
			OperatingHours:             p.OperatingHours,
			BankName:                   p.BankName,
			BankAccountNumber:          p.BankAccountNumber,
			BankAccountHolder:          p.BankAccountHolder,
			AllowedOrigins:             p.AllowedOrigins,
		})
		return err
	})
	if err != nil {
		return Knobs{}, translate(err)
	}
	return Knobs(knobsOf(sqlcgen.GetSettingsRow(row))), nil
}

// knobsOf is the one place a database row becomes a Knobs.
//
// The int32/int split is a column detail -- the contract says integer, the
// column is int -- so it is converted here rather than leaking the choice to
// every caller.
func knobsOf(row sqlcgen.GetSettingsRow) Knobs {
	return Knobs{
		Slug:                       row.Slug,
		BookingCodePrefix:          row.BookingCodePrefix,
		RequirePaymentBeforePickup: row.RequirePaymentBeforePickup,
		DraftExpiryHours:           int(row.DraftExpiryHours),
		PaymentDueHours:            int(row.PaymentDueHours),
		NoShowToleranceHours:       int(row.NoShowToleranceHours),
		NotifyPickupReminder:       row.NotifyPickupReminder,
		NotifyReturnReminder:       row.NotifyReturnReminder,
		NotifyOverdueReminder:      row.NotifyOverdueReminder,
		WhatsApp:                   row.Whatsapp,
		Address:                    row.Address,
		OperatingHours:             row.OperatingHours,
		BankName:                   row.BankName,
		BankAccountNumber:          row.BankAccountNumber,
		BankAccountHolder:          row.BankAccountHolder,
		AllowedOrigins:             row.AllowedOrigins,
	}
}

func int32Ptr(v *int) *int32 {
	if v == nil {
		return nil
	}
	n := int32(*v) //nolint:gosec // bounded by the schema's minimum and the column's CHECK
	return &n
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
	case "owners_whatsapp_format":
		// The one profile field a machine reads rather than a person: the
		// public page turns it into a wa.me link, and a freely formatted
		// number produces a dead link on the page whose whole purpose is
		// reaching the owner (BR-096).
		return apperrors.ValidationFailed(
			"A WhatsApp number starts with +62 and has 8 to 13 digits after it, " +
				"for example +628123456789.").
			WithFields(apperrors.Field{Name: "whatsapp"}).WithCause(err)

	case "owners_bank_account_number_format":
		return apperrors.ValidationFailed(
			"bank_account_number is 5 to 20 digits, with no spaces or dots.").
			WithFields(apperrors.Field{Name: "bank_account_number"}).WithCause(err)

	case "owners_no_show_tolerance_non_negative":
		return apperrors.ValidationFailed(
			"no_show_tolerance_hours cannot be negative. Zero is allowed and means the " +
				"unit is freed as soon as the booking starts.").WithCause(err)
	}
	return fmt.Errorf("update settings: %w", err)
}

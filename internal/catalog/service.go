// Package catalog holds what a rental has to rent out.  (S1-014 .. S1-017)
//
// Two tables, one package. BR-010 splits the catalogue in two -- `resources` is
// the kind of thing, `resource_units` is the thing -- but they share an owner,
// a foreign key and a single isolation file, so splitting the Go package would
// buy a boundary nobody crosses. Split it when units grow a state machine of
// their own.
//
// Everything here goes through InOwnerTx, so no query in db/queries/catalog.sql
// has a `WHERE owner_id`. RLS filters; another rental's id returns zero rows,
// which this package turns into the same 404 as an id that never existed. The
// difference between those two answers is how one rental's catalogue leaks
// (BR-001).
package catalog

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

type Service struct {
	store *db.Store
}

func New(store *db.Store) *Service { return &Service{store: store} }

// Optional is a value that knows whether its key was there at all.
//
// This exists for exactly four fields, and BR-016 is the whole reason. For
// `deposit_amount` and its three siblings, "key absent" and "key present and
// null" are DIFFERENT instructions: the first leaves the stored value alone,
// the second revokes it -- and revoking is the only way to say "this resource
// stopped taking a deposit" (04-api-spec.md section 3.2).
//
// A plain *int64 collapses both into nil and silently makes the second
// impossible. Everywhere else in this codebase a nil pointer does mean "absent",
// and that is still right; these four are the documented exception.
type Optional[T any] struct {
	Set   bool
	Value *T
}

// Clear reports the two halves the UPDATE needs: whether to touch the column at
// all, and what to put there.
func (o Optional[T]) Clear() (set bool, value *T) { return o.Set, o.Value }

// notFound is the same answer for a row that does not exist and a row that
// belongs to somebody else. Telling them apart is what leaks a competitor's
// catalogue one 404 at a time (BR-001, BR-030).
func notFoundResource() error { return apperrors.NotFound("No such resource in this business.") }
func notFoundUnit() error     { return apperrors.NotFound("No such unit in this business.") }

// translate turns a constraint violation into the code the contract names.
//
// Constraint names, not the message text: the message belongs to PostgreSQL and
// changes with its version, the name belongs to our migration.
//
// Note what is NOT here: a Go copy of each rule. The CHECKs in 000007 already
// refuse a zero deposit and an inverted duration range; restating them would
// give two places to disagree, and the one that drifts is the one nobody reads.
// This function only translates the refusal.
func translate(err error, verb string) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("%s: %w", verb, err)
	}

	switch pgErr.ConstraintName {
	case "resources_deposit_positive":
		return zeroIsNotEmpty("deposit_amount", "deposit").WithCause(err)
	case "resources_late_fee_positive":
		return zeroIsNotEmpty("late_fee_per_unit", "late fee").WithCause(err)
	case "resources_min_duration_positive":
		return zeroIsNotEmpty("min_duration", "minimum duration").WithCause(err)
	case "resources_max_duration_positive":
		return zeroIsNotEmpty("max_duration", "maximum duration").WithCause(err)

	case "resources_duration_order":
		return apperrors.ValidationFailed(
			"max_duration cannot be shorter than min_duration.").
			WithFields(apperrors.Field{Name: "max_duration"}).WithCause(err)

	case "resources_base_price_nonneg":
		return apperrors.ValidationFailed("base_price cannot be negative.").
			WithFields(apperrors.Field{Name: "base_price"}).WithCause(err)

	case "resources_buffer_nonneg":
		return apperrors.ValidationFailed("buffer_minutes cannot be negative.").
			WithFields(apperrors.Field{Name: "buffer_minutes"}).WithCause(err)

	case "resources_pricing_unit_valid":
		// Unreachable from the API: pricing_unit is not in any request schema,
		// so this can only fire if pricingUnitFor returned something the column
		// refuses -- a bug in this package, not in the caller's body.
		return fmt.Errorf("%s: server chose an invalid pricing unit: %w", verb, err)

	case "resources_status_valid", "resource_units_status_valid":
		return apperrors.ValidationFailed("That status is not one this system has.").
			WithFields(apperrors.Field{Name: "status"}).WithCause(err)

	case "resource_units_code_per_owner":
		// No `unit-code-taken` in the error catalogue (04-api-spec.md section 2),
		// and inventing one would make two catalogues to keep in step. A
		// field-level 422 says the same thing where the person can act on it.
		return apperrors.ValidationFailed(
			"That code is already used by another unit in this business.").
			WithFields(apperrors.Field{Name: "code"}).WithCause(err)

	// BR-094. Pesannya menjelaskan aturan lintas kolomnya, bukan menyebut nama
	// constraint -- orang yang membacanya sedang mengisi form, bukan membaca
	// migrasi.
	case "vehicle_specs_seats_car":
		return apperrors.ValidationFailed(
			"A car needs a seat count and a motorcycle cannot have one.").
			WithFields(apperrors.Field{Name: "vehicle.seats"}).WithCause(err)
	case "vehicle_specs_clutch_moto":
		return apperrors.ValidationFailed(
			"A clutch transmission only exists on a motorcycle.").
			WithFields(apperrors.Field{Name: "vehicle.transmission"}).WithCause(err)
	case "vehicle_specs_diesel_car":
		return apperrors.ValidationFailed(
			"Diesel only applies to a car.").
			WithFields(apperrors.Field{Name: "vehicle.fuel"}).WithCause(err)
	case "vehicle_specs_seats_range":
		return apperrors.ValidationFailed(
			"A seat count is between 2 and 20.").
			WithFields(apperrors.Field{Name: "vehicle.seats"}).WithCause(err)
	case "vehicle_specs_type_valid", "vehicle_specs_transmission_valid",
		"vehicle_specs_fuel_valid":
		return apperrors.ValidationFailed(
			"That is not a vehicle type, transmission or fuel this system has.").
			WithFields(apperrors.Field{Name: "vehicle"}).WithCause(err)
	case "vehicle_unit_details_year_range":
		return apperrors.ValidationFailed(
			"A model year is 1990 or later.").
			WithFields(apperrors.Field{Name: "vehicle.year"}).WithCause(err)

	// BR-095. Halaman publik merendernya apa adanya, jadi batasnya dijaga
	// database rather than trusting a maxLength nobody enforces.
	case "resources_description_length":
		return tooLong("description", 500).WithCause(err)
	case "resources_terms_excludes_length":
		return tooLong("terms_excludes", 500).WithCause(err)
	case "resources_terms_requirements_length":
		return tooLong("terms_requirements", 1000).WithCause(err)
	case "resources_terms_cancellation_length":
		return tooLong("terms_cancellation", 1000).WithCause(err)

	case "vehicle_specs_resource_matches_owner", "vehicle_unit_details_unit_matches_owner":
		// The composite key refusing means the parent belongs to another
		// rental. Same answer as a parent that does not exist (BR-001).
		return notFoundResource()

	case "resource_units_resource_matches_owner":
		// The composite key refusing means the resource id belongs to another
		// rental. Same answer as a resource that does not exist (BR-001).
		return notFoundResource()
	}
	return fmt.Errorf("%s: %w", verb, err)
}

// zeroIsNotEmpty is the message BR-016 actually wants the person to read.
//
// PRD A1 phrases the requirement as a sentence: "sistem menolak dan menyuruh
// saya mengosongkannya saja". Saying only "must be greater than zero" invites
// them to type 1, which is a real deposit of one rupiah.
func zeroIsNotEmpty(field, noun string) *apperrors.Error {
	return apperrors.ValidationFailed(
		"Leave " + field + " empty to run this resource without a " + noun +
			". Zero is not the way to say that -- a report cannot tell a " + noun +
			" of nothing from no " + noun + " at all.").
		WithFields(apperrors.Field{Name: field})
}

func tooLong(field string, max int) *apperrors.Error {
	return apperrors.ValidationFailed(
		field + " is at most " + strconv.Itoa(max) + " characters.").
		WithFields(apperrors.Field{Name: field})
}

// noRows reports whether err is the "nothing matched" that every id-taking query
// here returns for another rental's id.
func noRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// ActiveBookings counts the running bookings still holding this resource's old
// prices, because a price change never reaches a booking already made (BR-014).
//
// Always 0 today: `bookings` arrives with S1-022. The function exists now so the
// response shape is settled once and S1-018's screen is written once -- what
// lands with S1-022 is this body, not its callers.
func (s *Service) ActiveBookings(_ context.Context, _ uuid.UUID) (int, error) {
	return 0, nil
}

// AffectedBookings lists the bookings a unit's status change does NOT cancel.
//
// The list is the whole point of BR-013: the system warns and the owner
// decides. Refusing the change instead would make a juragan cancel bookings
// one by one before being allowed to say the car is in the workshop.
//
// Always empty today, for the same reason as ActiveBookings.
func (s *Service) AffectedBookings(_ context.Context, _ uuid.UUID) ([]AffectedBooking, error) {
	return []AffectedBooking{}, nil
}

// AffectedBooking is one booking that survives a unit going out of service.
type AffectedBooking struct {
	Code    string
	StartAt time.Time
	EndAt   time.Time
	Status  string
}

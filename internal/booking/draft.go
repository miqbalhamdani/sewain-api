package booking

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// NewDraft is a request from the public page: a resource, not a unit -- the
// renter never sees units (BR-025).
type NewDraft struct {
	ResourceID, CustomerID uuid.UUID
	StartAt, EndAt         time.Time
}

// CreateDraft records a public request as a draft.  (S1-051, BR-026, BR-027)
//
// The server picks the unit: the first one free right now. The draft does not
// hold it -- bookings_no_overlap ignores drafts -- so Confirm re-checks, and
// that is the point (BR-026). No unit free means NothingAvailable, never the
// backoffice's conflict list. No invoice: reserved is where due_at starts.
func (s *Service) CreateDraft(ctx context.Context, d NewDraft) (Booking, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return Booking{}, db.ErrNoOwnerContext
	}
	if err := checkRange(d.StartAt, d.EndAt, "end_at"); err != nil {
		return Booking{}, err
	}
	if !d.StartAt.After(time.Now()) {
		return Booking{}, apperrors.ValidationFailed("start_at is in the past.").
			WithFields(apperrors.Field{Name: "start_at"})
	}
	id := uuid.Must(uuid.NewV7())

	var row sqlcgen.GetBookingRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		free, err := q.ListAvailableUnits(ctx, sqlcgen.ListAvailableUnitsParams{
			ResourceID: &d.ResourceID, StartAt: d.StartAt, EndAt: d.EndAt})
		if err != nil {
			return err
		}
		if len(free) == 0 {
			return apperrors.NothingAvailable()
		}
		unit, err := q.GetBookableUnit(ctx, free[0].ID)
		if err != nil {
			return err
		}

		blacklisted, err := q.GetCustomerBlacklisted(ctx, d.CustomerID)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFoundCustomer()
		}
		if err != nil {
			return err
		}
		if blacklisted {
			return apperrors.CustomerBlacklisted()
		}

		qty, err := durationQty(d.StartAt, d.EndAt, unit.PricingUnit)
		if err != nil {
			return err
		}
		if err := checkDuration(qty, unit.MinDuration, unit.MaxDuration, unit.PricingUnit); err != nil {
			return err
		}

		number, err := q.NextBookingNumber(ctx, ownerID)
		if err != nil {
			return err
		}
		prefix, err := q.GetBookingCodePrefix(ctx, ownerID)
		if err != nil {
			return err
		}
		hours, err := q.GetDraftExpiryHours(ctx, ownerID)
		if err != nil {
			return err
		}
		expires := time.Now().Add(time.Duration(hours) * time.Hour)

		if err := q.InsertBooking(ctx, sqlcgen.InsertBookingParams{
			ID: id, OwnerID: ownerID,
			Code:       fmt.Sprintf("%s-%04d", prefix, number),
			CustomerID: d.CustomerID, ResourceID: unit.ResourceID, ResourceUnitID: unit.UnitID,
			StartAt: d.StartAt, EndAt: d.EndAt,
			Status: "draft", Source: "public_page",
			UnitPrice: unit.BasePrice, PricingUnit: unit.PricingUnit,
			BufferMinutes: unit.BufferMinutes, DurationQty: qty,
			Subtotal:       int64(qty) * unit.BasePrice,
			DepositAmount:  unit.DepositAmount,
			LateFeePerUnit: unit.LateFeePerUnit,
			ExpiresAt:      &expires,
		}); err != nil {
			return err
		}
		row, err = q.GetBooking(ctx, id)
		return err
	})
	if err != nil {
		return Booking{}, fmt.Errorf("create draft: %w", err)
	}
	return bookingOf(row), nil
}

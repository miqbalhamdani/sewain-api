package booking

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// Availability is one resource and its units free over a range (BR-020).
type Availability struct {
	ResourceID    uuid.UUID
	ResourceName  string
	BasePrice     int64
	PricingUnit   string
	DepositAmount *int64
	BufferMinutes int32
	Units         []UnitRef
	DurationQty   int32
	Subtotal      int64
}

type UnitRef struct {
	ID    uuid.UUID
	Code  string
	Label *string
}

// maxRange bounds availability and the calendar. A year is the PRD's own scale
// (500 units x 12 months); past it a request is a mistake, not a use.
const maxRange = 366 * 24 * time.Hour

// Availability is computed per request, no cache (03-erd.md section 4). A
// resource with nothing free still appears, with no units, so the screen says
// "full" instead of saying nothing.
func (s *Service) Availability(ctx context.Context, start, end time.Time, resourceID *uuid.UUID) ([]Availability, error) {
	if err := checkRange(start, end, "end_at"); err != nil {
		return nil, err
	}
	var resources []sqlcgen.ListBookableResourcesRow
	var units []sqlcgen.ListAvailableUnitsRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var err error
		if resources, err = q.ListBookableResources(ctx, resourceID); err != nil {
			return err
		}
		units, err = q.ListAvailableUnits(ctx, sqlcgen.ListAvailableUnitsParams{
			ResourceID: resourceID, StartAt: start, EndAt: end,
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("availability: %w", err)
	}

	byResource := make(map[uuid.UUID][]UnitRef, len(resources))
	for _, u := range units {
		byResource[u.ResourceID] = append(byResource[u.ResourceID], UnitRef{ID: u.ID, Code: u.Code, Label: u.Label})
	}
	out := make([]Availability, 0, len(resources))
	for _, r := range resources {
		qty, err := durationQty(start, end, r.PricingUnit)
		if err != nil {
			return nil, err
		}
		free := byResource[r.ID]
		if free == nil {
			free = []UnitRef{}
		}
		out = append(out, Availability{
			ResourceID: r.ID, ResourceName: r.Name, BasePrice: r.BasePrice,
			PricingUnit: r.PricingUnit, DepositAmount: r.DepositAmount,
			BufferMinutes: r.BufferMinutes, Units: free,
			DurationQty: qty, Subtotal: int64(qty) * r.BasePrice,
		})
	}
	return out, nil
}

func checkRange(start, end time.Time, field string) error {
	if !end.After(start) {
		return apperrors.ValidationFailed(field + " must be after the start of the range.").
			WithFields(apperrors.Field{Name: field})
	}
	if end.Sub(start) > maxRange {
		return apperrors.ValidationFailed("A range is at most 366 days.").
			WithFields(apperrors.Field{Name: field})
	}
	return nil
}

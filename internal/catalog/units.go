package catalog

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
)

// Unit is one physical thing. Three Avanzas are one Resource and three of these,
// and a booking always names one of these -- never just the kind (BR-010).
type Unit struct {
	ID         uuid.UUID
	ResourceID uuid.UUID

	// Plate or serial number, typed by a person. Unique per rental, not global.
	// Not to be confused with bookings.code, which the server generates.
	Code           string
	Label          *string
	Status         string
	MeterValue     *int64
	ConditionNotes *string
}

// NewUnit is what a caller is adding. Status is absent: a unit is born active,
// and moving it out of service is a separate act with its own warning (BR-013).
type NewUnit struct {
	Code           string
	Label          *string
	MeterValue     *int64
	ConditionNotes *string
}

// UnitPatch is what a caller is changing.
//
// No Optional fields here, unlike ResourcePatch: none of these is a BR-016
// nominal, so there is nothing whose absence has to be told apart from null.
type UnitPatch struct {
	Code           *string
	Label          *string
	Status         *string
	MeterValue     *int64
	ConditionNotes *string
}

// ListUnits returns the units of one resource.
//
// The resource is checked first, and a missing one is a 404 rather than an
// empty list: "this kind has no units" and "this kind is not yours" are
// different answers, and only one of them should make a screen say "add one".
func (s *Service) ListUnits(ctx context.Context, resourceID uuid.UUID) ([]Unit, error) {
	var rows []sqlcgen.ListUnitsRow
	var exists bool
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)

		var err error
		if exists, err = q.ResourceExists(ctx, resourceID); err != nil || !exists {
			return err
		}
		rows, err = q.ListUnits(ctx, resourceID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list units: %w", err)
	}
	if !exists {
		return nil, notFoundResource()
	}

	out := make([]Unit, 0, len(rows))
	for _, row := range rows {
		out = append(out, unitOf(sqlcgen.CreateUnitRow(row)))
	}
	return out, nil
}

// CreateUnit adds a physical thing to a kind.
func (s *Service) CreateUnit(ctx context.Context, userID, resourceID uuid.UUID, n NewUnit) (Unit, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return Unit{}, db.ErrNoOwnerContext
	}

	id, err := uuid.NewV7()
	if err != nil {
		return Unit{}, fmt.Errorf("new unit id: %w", err)
	}

	var row sqlcgen.CreateUnitRow
	var exists bool
	err = s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)

		// Checked rather than left to the composite foreign key, only so the
		// answer is a 404 about the resource instead of a 422 about a
		// constraint the caller has never heard of. The key is still what makes
		// it impossible (BR-001).
		var err error
		if exists, err = q.ResourceExists(ctx, resourceID); err != nil || !exists {
			return err
		}

		row, err = q.CreateUnit(ctx, sqlcgen.CreateUnitParams{
			ID:             id,
			OwnerID:        ownerID,
			CreatedBy:      &userID,
			ResourceID:     resourceID,
			Code:           n.Code,
			Label:          n.Label,
			MeterValue:     n.MeterValue,
			ConditionNotes: n.ConditionNotes,
		})
		return err
	})
	if err != nil {
		return Unit{}, translate(err, "create unit")
	}
	if !exists {
		return Unit{}, notFoundResource()
	}
	return unitOf(row), nil
}

// UpdateUnit applies the fields the caller sent, including a status change.
//
// It never refuses because of existing bookings and never cancels one. The
// caller pairs this with AffectedBookings and shows the owner what is affected;
// the owner decides (BR-013).
func (s *Service) UpdateUnit(ctx context.Context, id uuid.UUID, p UnitPatch) (Unit, error) {
	var row sqlcgen.UpdateUnitRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).UpdateUnit(ctx, sqlcgen.UpdateUnitParams{
			ID:             id,
			Code:           p.Code,
			Label:          p.Label,
			Status:         p.Status,
			MeterValue:     p.MeterValue,
			ConditionNotes: p.ConditionNotes,
		})
		return err
	})
	if noRows(err) {
		return Unit{}, notFoundUnit()
	}
	if err != nil {
		return Unit{}, translate(err, "update unit")
	}
	return unitOf(sqlcgen.CreateUnitRow(row)), nil
}

// DeleteUnit soft-deletes one unit, which releases its code again (BR-011).
//
// Returns false when the unit is not in this rental, so the caller can answer
// 404; an already-deleted unit is a success.
func (s *Service) DeleteUnit(ctx context.Context, id uuid.UUID) (bool, error) {
	var exists bool
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)

		var err error
		if exists, err = q.UnitExists(ctx, id); err != nil || !exists {
			return err
		}
		_, err = q.SoftDeleteUnit(ctx, id)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("delete unit: %w", err)
	}
	return exists, nil
}

// unitOf is the one place a database row becomes a Unit.
func unitOf(row sqlcgen.CreateUnitRow) Unit {
	return Unit{
		ID:             row.ID,
		ResourceID:     row.ResourceID,
		Code:           row.Code,
		Label:          row.Label,
		Status:         row.Status,
		MeterValue:     row.MeterValue,
		ConditionNotes: row.ConditionNotes,
	}
}

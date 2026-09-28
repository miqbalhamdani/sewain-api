package catalog

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// Resource is one kind of thing as this package talks about it.
//
// Not the sqlc row type: that one is regenerated from the migrations, so
// letting it out of this package would make a column rename ripple into the
// HTTP layer. The conversion lives here, once.
type Resource struct {
	ID          uuid.UUID
	Name        string
	Category    *string
	PricingUnit string
	BasePrice   int64

	// The four BR-016 nominals. nil means the rule does not apply to this
	// resource -- no deposit at all, rather than a deposit of zero. The
	// database refuses 0 so the two can never be confused.
	DepositAmount  *int64
	LateFeePerUnit *int64
	MinDuration    *int32
	MaxDuration    *int32

	BufferMinutes          int32
	RequiresIDVerification bool
	Status                 string

	// Active units. Zero means this resource can never appear in availability
	// search, whatever its own status says (BR-010).
	UnitCount int64
}

// NewResource is what a caller is creating.
//
// PricingUnit is absent on purpose and cannot be added: the server reads it
// from the owner's preset (BR-017), and the contract has no field for it.
type NewResource struct {
	Name                   string
	Category               *string
	BasePrice              int64
	DepositAmount          *int64
	LateFeePerUnit         *int64
	MinDuration            *int32
	MaxDuration            *int32
	BufferMinutes          int32
	RequiresIDVerification bool
}

// ResourcePatch is what a caller is changing.
//
// Plain pointers mean "absent leaves it alone". The four Optional fields mean
// something more: absent leaves it alone, present-and-nil revokes it. See
// Optional's doc comment for why only these four.
type ResourcePatch struct {
	Name                   *string
	Category               *string
	BasePrice              *int64
	Status                 *string
	BufferMinutes          *int32
	RequiresIDVerification *bool

	DepositAmount  Optional[int64]
	LateFeePerUnit Optional[int64]
	MinDuration    Optional[int32]
	MaxDuration    Optional[int32]
}

// pricingUnitForPreset is BR-017's table, and the only copy of it.
//
// It is a constant rather than a column because the mapping is product
// configuration, not tenant data -- six presets need one source of truth, not
// one per rental (03-erd.md section 3). Changing a preset does not touch
// resources that already exist: the unit is decided at creation, like every
// other snapshot in this system (BR-014, BR-024).
//
// `clinic` is missing deliberately. PRD section 1 says its unit is "30 menit",
// which is not in BR-012's enum at all, and that gets decided in phase 4 rather
// than guessed here. An owner on that preset cannot create a resource yet, and
// saying so is better than filing their rentals under the wrong unit.
var pricingUnitForPreset = map[string]string{
	"vehicle_rental":   "day",
	"equipment_rental": "day",
	"boarding_house":   "month",
	"apartment":        "month",
	"venue":            "hour",
}

// List returns every resource in this rental, newest naming order first.
func (s *Service) List(ctx context.Context) ([]Resource, error) {
	var rows []sqlcgen.ListResourcesRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlcgen.New(tx).ListResources(ctx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list resources: %w", err)
	}

	// An empty slice, never nil: the contract types this as a list, and a
	// client that has to handle both shapes will handle one of them wrong.
	out := make([]Resource, 0, len(rows))
	for _, row := range rows {
		out = append(out, resourceOf(sqlcgen.GetResourceRow(row)))
	}
	return out, nil
}

// Get reads one resource. Another rental's id is a 404, identical to an id that
// was never issued.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Resource, error) {
	var row sqlcgen.GetResourceRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).GetResource(ctx, id)
		return err
	})
	if noRows(err) {
		return Resource{}, notFoundResource()
	}
	if err != nil {
		return Resource{}, fmt.Errorf("get resource: %w", err)
	}
	return resourceOf(row), nil
}

// Create registers a new kind of thing.
//
// Two statements in one transaction, and the order matters: the owner's preset
// decides the pricing unit (BR-017), so it is read before the insert rather
// than defaulted by the column. The column default exists as a second line of
// defence, not as the answer.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, n NewResource) (Resource, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return Resource{}, db.ErrNoOwnerContext
	}

	id, err := uuid.NewV7()
	if err != nil {
		return Resource{}, fmt.Errorf("new resource id: %w", err)
	}

	var row sqlcgen.GetResourceRow
	err = s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)

		preset, err := q.GetOwnerBusinessType(ctx, ownerID)
		if err != nil {
			return err
		}
		unit, known := pricingUnitForPreset[preset]
		if !known {
			return apperrors.ValidationFailed(
				"The pricing unit for the " + preset + " preset has not been decided yet, " +
					"so resources cannot be created on it.")
		}

		if _, err := q.CreateResource(ctx, sqlcgen.CreateResourceParams{
			ID:                     id,
			OwnerID:                ownerID,
			CreatedBy:              &userID,
			Name:                   n.Name,
			Category:               n.Category,
			PricingUnit:            unit,
			BasePrice:              n.BasePrice,
			DepositAmount:          n.DepositAmount,
			LateFeePerUnit:         n.LateFeePerUnit,
			MinDuration:            n.MinDuration,
			MaxDuration:            n.MaxDuration,
			BufferMinutes:          n.BufferMinutes,
			RequiresIDVerification: n.RequiresIDVerification,
		}); err != nil {
			return err
		}

		// Read it back rather than RETURNING it: unit_count is a subquery and
		// cannot ride on an INSERT, and one row shape with one converter beats
		// a second shape that differs by one column.
		row, err = q.GetResource(ctx, id)
		return err
	})
	if err != nil {
		return Resource{}, translate(err, "create resource")
	}
	return resourceOf(row), nil
}

// Update applies the fields the caller actually sent.
func (s *Service) Update(ctx context.Context, id uuid.UUID, p ResourcePatch) (Resource, error) {
	setDeposit, deposit := p.DepositAmount.Clear()
	setLateFee, lateFee := p.LateFeePerUnit.Clear()
	setMin, minDuration := p.MinDuration.Clear()
	setMax, maxDuration := p.MaxDuration.Clear()

	var row sqlcgen.GetResourceRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)

		if _, err := q.UpdateResource(ctx, sqlcgen.UpdateResourceParams{
			ID:                     id,
			Name:                   p.Name,
			Category:               p.Category,
			BasePrice:              p.BasePrice,
			Status:                 p.Status,
			BufferMinutes:          p.BufferMinutes,
			RequiresIDVerification: p.RequiresIDVerification,

			SetDepositAmount:  setDeposit,
			DepositAmount:     deposit,
			SetLateFeePerUnit: setLateFee,
			LateFeePerUnit:    lateFee,
			SetMinDuration:    setMin,
			MinDuration:       minDuration,
			SetMaxDuration:    setMax,
			MaxDuration:       maxDuration,
		}); err != nil {
			return err
		}

		var err error
		row, err = q.GetResource(ctx, id)
		return err
	})
	if noRows(err) {
		return Resource{}, notFoundResource()
	}
	if err != nil {
		return Resource{}, translate(err, "update resource")
	}
	return resourceOf(row), nil
}

// Delete soft-deletes a resource and, in the same transaction, its units.
//
// Cascading is not tidiness. A unit left behind belongs to a kind nothing can
// see any more, and it keeps holding its code -- the unique index only ignores
// deleted rows (BR-011). A plate that moves to another car has to be usable on
// that car, and it would not be.
//
// Returns false when the resource is not in this rental at all, so the caller
// can answer 404; an already-deleted resource is a success, because DELETE is
// allowed to be idempotent.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) (bool, error) {
	var exists bool
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)

		var err error
		if exists, err = q.ResourceExists(ctx, id); err != nil || !exists {
			return err
		}
		if _, err := q.SoftDeleteUnitsOfResource(ctx, id); err != nil {
			return err
		}
		_, err = q.SoftDeleteResource(ctx, id)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("delete resource: %w", err)
	}
	return exists, nil
}

// resourceOf is the one place a database row becomes a Resource.
//
// The int32/int split is a column detail -- the contract says integer, the
// column is int -- so it is converted at the edges rather than leaking the
// choice to every caller.
func resourceOf(row sqlcgen.GetResourceRow) Resource {
	return Resource{
		ID:                     row.ID,
		Name:                   row.Name,
		Category:               row.Category,
		PricingUnit:            row.PricingUnit,
		BasePrice:              row.BasePrice,
		DepositAmount:          row.DepositAmount,
		LateFeePerUnit:         row.LateFeePerUnit,
		MinDuration:            row.MinDuration,
		MaxDuration:            row.MaxDuration,
		BufferMinutes:          row.BufferMinutes,
		RequiresIDVerification: row.RequiresIDVerification,
		Status:                 row.Status,
		UnitCount:              row.UnitCount,
	}
}

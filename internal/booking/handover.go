package booking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
	"github.com/miqbalhamdani/sewain-api/internal/storage"
)

// Serah-terima.  (S1-034 .. S1-036)
//
// Both directions follow one shape, inside one transaction holding the booking
// row: decide from the locked row, THEN promote the photos out of pending/,
// THEN write. Promoting last means a request refused for its status, its
// odometer or a physical conflict never copies a byte -- and the row only ever
// points at an object HEAD has seen (BR-093).

// HandoverInput is what both directions share.
type HandoverInput struct {
	PhotoKeys      []string
	MeterValue     *int64
	Checklist      map[string]any
	ConditionNotes *string
}

type PickupInput struct {
	HandoverInput
	ConfirmPhysicalConflict bool
}

type ReturnInput struct {
	HandoverInput
	ConfirmLateFee bool
	LateFeeWaived  int64
	WaiverReason   string
	Damages        []Damage
}

// Damage names a photo by its pending key -- one of this request's own
// photo_keys -- because the photo has no id until this request writes it.
type Damage struct {
	Amount      int64
	Description string
	PhotoKey    string
}

// Preview is the late fee as it stands right now. It stores nothing.
type Preview struct {
	ActualReturnAt time.Time
	EndAt          time.Time
	OverdueUnits   int32
	PricingUnit    string
	LateFeePerUnit *int64
	LateFeeTotal   int64
	DepositAmount  *int64
}

// Pickup hands the unit over: reserved -> picked_up (BR-035). no_show is
// accepted too -- the renter who turns up late is not refused outright (BR-057).
func (s *Service) Pickup(ctx context.Context, userID, id uuid.UUID, in PickupInput) (Booking, error) {
	return s.handover(ctx, id, func(q *sqlcgen.Queries, ownerID uuid.UUID, b sqlcgen.LockBookingForHandoverRow) error {
		if b.Status != "reserved" && b.Status != "no_show" {
			return wrongStatus("Only a reserved booking can be picked up; this booking is " + b.Status + ".")
		}
		if err := checkHandover(in.HandoverInput, b.IsVehicle); err != nil {
			return err
		}
		if b.RequirePaymentBeforePickup {
			rent, err := q.RentInvoiceState(ctx, &id)
			if err != nil {
				return err
			}
			if rent.HasRent && !rent.Paid {
				return apperrors.PaymentRequiredBeforePickup()
			}
		}
		if !in.ConfirmPhysicalConflict {
			rows, err := q.FindPhysicalConflicts(ctx, sqlcgen.FindPhysicalConflictsParams{
				UnitID: b.ResourceUnitID, ExcludeID: id})
			if err != nil {
				return err
			}
			if len(rows) > 0 {
				conflicts := make([]apperrors.Conflict, 0, len(rows))
				for _, r := range rows {
					conflicts = append(conflicts, apperrors.Conflict{Code: r.Code, StartAt: r.StartAt, EndAt: r.EndAt, Status: r.Status})
				}
				return apperrors.PhysicalConflictUnconfirmed(conflicts)
			}
		}

		if _, err := s.writeHandover(ctx, q, ownerID, userID, id, "pickup", in.HandoverInput, nil, nil); err != nil {
			return err
		}
		if err := q.MarkPickedUp(ctx, id); err != nil {
			return err
		}
		return setMeter(ctx, q, b.ResourceUnitID, in.MeterValue)
	})
}

// ReturnPreview is BR-046 computed at this instant: ceil(overrun / pricing
// unit) x late_fee_per_unit, from the booking's own snapshot.
func (s *Service) ReturnPreview(ctx context.Context, id uuid.UUID, now time.Time) (Preview, error) {
	var p Preview
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		b, err := sqlcgen.New(tx).LockBookingForHandover(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFoundBooking()
		}
		if err != nil {
			return err
		}
		if b.Status != "picked_up" {
			return wrongStatus("Only a picked-up booking can be returned; this booking is " + b.Status + ".")
		}
		p, err = previewOf(b, now)
		return err
	})
	if err != nil {
		return Preview{}, translate(err, "return preview")
	}
	return p, nil
}

func previewOf(b sqlcgen.LockBookingForHandoverRow, now time.Time) (Preview, error) {
	p := Preview{ActualReturnAt: now, EndAt: b.EndAt, PricingUnit: b.PricingUnit,
		LateFeePerUnit: b.LateFeePerUnit, DepositAmount: b.DepositAmount}
	if now.After(b.EndAt) {
		units, err := durationQty(b.EndAt, now, b.PricingUnit)
		if err != nil {
			return Preview{}, err
		}
		p.OverdueUnits = units
	}
	// No rate means no fee line -- but the overrun is still counted, because
	// being late still happened (BR-016, BR-041).
	if b.LateFeePerUnit != nil {
		p.LateFeeTotal = int64(p.OverdueUnits) * *b.LateFeePerUnit
	}
	return p, nil
}

// Return takes the unit back: picked_up -> returned (BR-035, BR-040). The
// late fee and damages are the operator's decision, not a copy of the preview,
// and only what is confirmed is issued (BR-051).
func (s *Service) Return(ctx context.Context, userID, id uuid.UUID, in ReturnInput) (Booking, error) {
	now := time.Now()
	return s.handover(ctx, id, func(q *sqlcgen.Queries, ownerID uuid.UUID, b sqlcgen.LockBookingForHandoverRow) error {
		if b.Status != "picked_up" {
			return wrongStatus("Only a picked-up booking can be returned; this booking is " + b.Status + ".")
		}
		if err := checkHandover(in.HandoverInput, b.IsVehicle); err != nil {
			return err
		}
		p, err := previewOf(b, now)
		if err != nil {
			return err
		}

		// What is waived: the stated part, or all of it when the proposed fee
		// is not confirmed. Any waiver needs a reason, recorded on the handover
		// -- a full waiver has no invoice line to carry it (BR-051 duty 2).
		reason := strings.TrimSpace(in.WaiverReason)
		var waived int64
		switch {
		case p.LateFeeTotal == 0:
		case !in.ConfirmLateFee:
			waived = p.LateFeeTotal
		default:
			if in.LateFeeWaived < 0 || in.LateFeeWaived > p.LateFeeTotal {
				return apperrors.ValidationFailed("late_fee_waived must be between 0 and the late fee.").
					WithFields(apperrors.Field{Name: "late_fee_waived"})
			}
			waived = in.LateFeeWaived
		}
		if waived > 0 && reason == "" {
			return apperrors.WaiverReasonRequired()
		}

		own := make(map[string]bool, len(in.PhotoKeys))
		for _, k := range in.PhotoKeys {
			own[k] = true
		}
		for i, d := range in.Damages {
			field := fmt.Sprintf("damages[%d]", i)
			if !own[d.PhotoKey] {
				// BR-047: a damage line names a photo of THIS return.
				return apperrors.ValidationFailed("A damage must point at one of this return's photos.").
					WithFields(apperrors.Field{Name: field + ".photo_key"})
			}
			if d.Amount <= 0 || strings.TrimSpace(d.Description) == "" {
				return apperrors.ValidationFailed("A damage needs an amount above zero and a description.").
					WithFields(apperrors.Field{Name: field})
			}
		}

		var waivedPtr *int64
		var reasonPtr *string
		if waived > 0 {
			waivedPtr, reasonPtr = &waived, &reason
		}
		photoIDs, err := s.writeHandover(ctx, q, ownerID, userID, id, "return", in.HandoverInput, waivedPtr, reasonPtr)
		if err != nil {
			return err
		}

		var lines []line
		if charged := p.LateFeeTotal - waived; charged > 0 {
			lines = append(lines, line{kind: "late_fee", amount: charged, waiverReason: reasonPtr,
				description: LateFeeDescription(p)})
		}
		for _, d := range in.Damages {
			pid := photoIDs[d.PhotoKey]
			lines = append(lines, line{kind: "damage", amount: d.Amount,
				description: strings.TrimSpace(d.Description), photoID: &pid})
		}
		dueHours, err := q.GetPaymentDueHours(ctx, ownerID)
		if err != nil {
			return err
		}
		if err := issueInvoice(ctx, q, ownerID, userID, id, b.CustomerID, b.Code,
			now.Add(time.Duration(dueHours)*time.Hour), lines); err != nil {
			return err
		}

		if err := q.MarkReturned(ctx, sqlcgen.MarkReturnedParams{ID: id, ActualReturnAt: &now}); err != nil {
			return err
		}
		return setMeter(ctx, q, b.ResourceUnitID, in.MeterValue)
	})
}

// handover is the transaction both directions run in.
func (s *Service) handover(ctx context.Context, id uuid.UUID,
	act func(q *sqlcgen.Queries, ownerID uuid.UUID, b sqlcgen.LockBookingForHandoverRow) error) (Booking, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return Booking{}, db.ErrNoOwnerContext
	}
	var row sqlcgen.GetBookingRow
	var locked sqlcgen.LockBookingForHandoverRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var err error
		if locked, err = q.LockBookingForHandover(ctx, id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return notFoundBooking()
			}
			return err
		}
		if err := act(q, ownerID, locked); err != nil {
			return err
		}
		row, err = q.GetBooking(ctx, id)
		return err
	})
	if err != nil {
		// A no_show picked up onto a unit someone else now holds lands on
		// bookings_no_overlap -- the same booking-conflict as anywhere else.
		return Booking{}, s.answer(ctx, err, "handover", locked.ResourceUnitID, id,
			locked.StartAt, locked.EndAt, 0)
	}
	return bookingOf(row), nil
}

// writeHandover promotes the photos and writes the handover with them,
// returning pending key -> photo id for the damage lines that point at them.
func (s *Service) writeHandover(ctx context.Context, q *sqlcgen.Queries, ownerID, userID, bookingID uuid.UUID,
	direction string, in HandoverInput, waived *int64, reason *string) (map[string]uuid.UUID, error) {
	checklist, err := json.Marshal(in.Checklist)
	if err != nil || in.Checklist == nil {
		checklist = []byte(`{}`)
	}
	handoverID := uuid.Must(uuid.NewV7())
	if err := q.InsertHandover(ctx, sqlcgen.InsertHandoverParams{
		ID: handoverID, OwnerID: ownerID, BookingID: bookingID, Direction: direction,
		PerformedBy: userID, MeterValue: in.MeterValue, Checklist: checklist,
		ConditionNotes: in.ConditionNotes, LateFeeWaived: waived, WaiverReason: reason,
	}); err != nil {
		return nil, err
	}
	prefix := "handovers/" + ownerID.String() + "/" + bookingID.String()
	ids := make(map[string]uuid.UUID, len(in.PhotoKeys))
	for i, key := range in.PhotoKeys {
		if _, dup := ids[key]; dup {
			continue
		}
		final, err := s.objects.Promote(ctx, ownerID, key, prefix, fmt.Sprintf("photo_keys[%d]", i))
		if err != nil {
			return nil, err
		}
		photoID := uuid.Must(uuid.NewV7())
		if err := q.InsertHandoverPhoto(ctx, sqlcgen.InsertHandoverPhotoParams{
			ID: photoID, OwnerID: ownerID, HandoverID: handoverID, ObjectKey: final,
		}); err != nil {
			return nil, err
		}
		ids[key] = photoID
	}
	return ids, nil
}

// checkHandover is BR-036: at least one photo, and the odometer for a vehicle.
func checkHandover(in HandoverInput, isVehicle bool) error {
	if len(in.PhotoKeys) == 0 {
		return apperrors.HandoverPhotoRequired()
	}
	if isVehicle && in.MeterValue == nil {
		return apperrors.MeterValueRequired()
	}
	return nil
}

func setMeter(ctx context.Context, q *sqlcgen.Queries, unitID uuid.UUID, meter *int64) error {
	if meter == nil {
		return nil
	}
	return q.SetUnitMeter(ctx, sqlcgen.SetUnitMeterParams{ID: unitID, MeterValue: meter})
}

// Handover is one recorded handover with signed photo URLs.
type Handover struct {
	ID             uuid.UUID
	Direction      string
	PerformedBy    string
	PerformedAt    time.Time
	MeterValue     *int64
	Checklist      map[string]any
	ConditionNotes *string
	LateFeeWaived  *int64
	WaiverReason   *string
	Photos         []Photo
}

type Photo struct {
	ID         uuid.UUID
	URL        string
	CapturedAt time.Time
}

// Handovers lists a booking's evidence, read-only for every role (BR-037).
func (s *Service) Handovers(ctx context.Context, bookingID uuid.UUID) ([]Handover, error) {
	var hs []sqlcgen.ListHandoversRow
	var ps []sqlcgen.ListHandoverPhotosRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if _, err := q.GetBooking(ctx, bookingID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return notFoundBooking()
			}
			return err
		}
		var err error
		if hs, err = q.ListHandovers(ctx, bookingID); err != nil {
			return err
		}
		ps, err = q.ListHandoverPhotos(ctx, bookingID)
		return err
	})
	if err != nil {
		return nil, translate(err, "list handovers")
	}
	byHandover := map[uuid.UUID][]Photo{}
	for _, p := range ps {
		url, err := s.objects.PresignGet(ctx, p.ObjectKey, storage.HandoverPhotoTTL)
		if err != nil {
			return nil, err
		}
		byHandover[p.HandoverID] = append(byHandover[p.HandoverID], Photo{ID: p.ID, URL: url, CapturedAt: p.CapturedAt})
	}
	out := make([]Handover, 0, len(hs))
	for _, h := range hs {
		checklist := map[string]any{}
		_ = json.Unmarshal(h.Checklist, &checklist)
		photos := byHandover[h.ID]
		if photos == nil {
			photos = []Photo{}
		}
		out = append(out, Handover{ID: h.ID, Direction: h.Direction, PerformedBy: h.PerformedBy,
			PerformedAt: h.PerformedAt, MeterValue: h.MeterValue, Checklist: checklist,
			ConditionNotes: h.ConditionNotes, LateFeeWaived: h.LateFeeWaived,
			WaiverReason: h.WaiverReason, Photos: photos})
	}
	return out, nil
}

// LateFeeDescription is the line text a renter reads: "Telat 2 hari".
func LateFeeDescription(p Preview) string {
	return fmt.Sprintf("Telat %d %s", p.OverdueUnits, unitName[p.PricingUnit])
}

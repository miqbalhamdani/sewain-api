package booking

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// Deposit settlement.  (S1-042, BR-048 .. BR-051)
//
// The deposit ABSORBS the charges issued at return. Return already invoiced
// the late fee and damage; settling then deducting the same amounts from the
// deposit would bill the renter twice. So settle cancels the still-unpaid
// return invoice -- its lines stay as the itemised record -- records what the
// deposit covered and what goes back, and issues a new invoice only for what
// the deposit could not cover. A negative deposit never exists (BR-048).

// charge is one late-fee or damage line the deposit can absorb.
type charge struct {
	kind, description string
	amount            int64
	photoID           *uuid.UUID
}

// absorb applies the deposit to the charges in order. deducted + refunded is
// always exactly the deposit; rest is what the deposit did not cover, line by
// line, so a damage line keeps its photo on the shortfall invoice (BR-047).
func absorb(deposit int64, charges []charge) (deducted, refunded int64, rest []line) {
	left := deposit
	for _, c := range charges {
		take := min(left, c.amount)
		left -= take
		deducted += take
		if over := c.amount - take; over > 0 {
			desc := c.description
			if take > 0 {
				desc += " (sisa sesudah deposit)"
			}
			rest = append(rest, line{kind: c.kind, amount: over, description: desc, photoID: c.photoID})
		}
	}
	return deducted, deposit - deducted, rest
}

// DepositPreview is the settlement as it would be right now. Stores nothing.
type DepositPreview struct {
	DepositAmount    *int64 // nil: no deposit, or waived
	Collected        bool
	Waived           bool
	SettledAt        *time.Time
	Deductions       int64
	RefundAmount     int64
	DeductedAmount   int64
	NewInvoiceAmount int64
}

func (s *Service) DepositPreview(ctx context.Context, id uuid.UUID) (DepositPreview, error) {
	var p DepositPreview
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		b, err := q.LockBookingDeposit(ctx, id)
		if err != nil {
			return err
		}
		p.Waived, p.SettledAt = b.DepositWaivedAt != nil, b.DepositSettledAt
		if b.DepositAmount == nil || p.Waived {
			return nil // BR-016: nothing to settle; complete is not blocked
		}
		p.DepositAmount = b.DepositAmount
		inv, err := q.DepositInvoice(ctx, &id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		p.Collected = err == nil && inv.Status == "paid"
		charges, err := returnCharges(ctx, q, id)
		if err != nil {
			return err
		}
		for _, c := range charges {
			p.Deductions += c.amount
		}
		var rest []line
		p.DeductedAmount, p.RefundAmount, rest = absorb(*b.DepositAmount, charges)
		for _, l := range rest {
			p.NewInvoiceAmount += l.amount
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DepositPreview{}, notFoundBooking()
	}
	if err != nil {
		return DepositPreview{}, translate(err, "deposit preview")
	}
	return p, nil
}

// Settle is BR-048 written down: deducted + refunded = deposit, the return
// invoice cancelled, any shortfall issued fresh. The refund itself is paid out
// of the system; this records its amount.
func (s *Service) Settle(ctx context.Context, userID, id uuid.UUID, note string) (Booking, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return Booking{}, db.ErrNoOwnerContext
	}
	note = strings.TrimSpace(note)
	return s.depositTx(ctx, id, func(q *sqlcgen.Queries, b sqlcgen.LockBookingDepositRow) error {
		if b.Status != "returned" {
			return wrongStatus("A deposit is settled after return; this booking is " + b.Status + ".")
		}
		if b.DepositAmount == nil || b.DepositWaivedAt != nil {
			return apperrors.DepositNotApplicable()
		}
		if b.DepositSettledAt != nil {
			return wrongStatus("This deposit is already settled.")
		}
		inv, err := q.DepositInvoice(ctx, &id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && inv.Status != "paid") {
			return apperrors.DepositNotCollected()
		}
		if err != nil {
			return err
		}
		charges, err := returnCharges(ctx, q, id)
		if err != nil {
			return err
		}
		deducted, refunded, rest := absorb(*b.DepositAmount, charges)
		if deducted > 0 && note == "" {
			// BR-049: a deduction is settled "with a reason note".
			return apperrors.ValidationFailed("A deduction needs a note saying what it was for.").
				WithFields(apperrors.Field{Name: "note"})
		}

		cancelled := map[uuid.UUID]bool{}
		rows, err := q.ReturnCharges(ctx, &id)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if !cancelled[r.InvoiceID] {
				if err := q.CancelInvoice(ctx, r.InvoiceID); err != nil {
					return err
				}
				cancelled[r.InvoiceID] = true
			}
		}
		var notePtr *string
		if note != "" {
			notePtr = &note
		}
		if err := q.SettleDeposit(ctx, sqlcgen.SettleDepositParams{
			ID: id, DepositDeducted: deducted, DepositRefunded: refunded, DepositNote: notePtr,
		}); err != nil {
			return err
		}
		dueHours, err := q.GetPaymentDueHours(ctx, ownerID)
		if err != nil {
			return err
		}
		return issueInvoice(ctx, q, ownerID, userID, id, b.CustomerID, b.Code,
			time.Now().Add(time.Duration(dueHours)*time.Hour), rest)
	})
}

// Complete is returned -> completed, blocked only by a deposit that exists, was
// not waived, and is not settled (BR-049). No deposit never blocks (BR-016).
func (s *Service) Complete(ctx context.Context, id uuid.UUID) (Booking, error) {
	return s.depositTx(ctx, id, func(q *sqlcgen.Queries, b sqlcgen.LockBookingDepositRow) error {
		if b.Status != "returned" {
			return wrongStatus("Only a returned booking can be completed; this booking is " + b.Status + ".")
		}
		if b.DepositAmount != nil && b.DepositWaivedAt == nil && b.DepositSettledAt == nil {
			return apperrors.DepositNotSettled()
		}
		return q.CompleteBooking(ctx, id)
	})
}

// Waive gives up the deposit while it is still unpaid (BR-051, owner only as
// decided in M4). The line is removed, not offset with a discount; who, when
// and why go on the booking and into audit_logs.
func (s *Service) Waive(ctx context.Context, userID, id uuid.UUID, reason string) (Booking, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return Booking{}, db.ErrNoOwnerContext
	}
	reason = strings.TrimSpace(reason)
	return s.depositTx(ctx, id, func(q *sqlcgen.Queries, b sqlcgen.LockBookingDepositRow) error {
		if b.DepositAmount == nil || b.DepositWaivedAt != nil {
			return apperrors.DepositNotApplicable()
		}
		if reason == "" {
			return apperrors.WaiverReasonRequired()
		}
		if b.Status == "cancelled" || b.Status == "completed" {
			return wrongStatus("A " + b.Status + " booking has no deposit left to waive.")
		}
		inv, err := q.DepositInvoice(ctx, &id)
		if errors.Is(err, pgx.ErrNoRows) {
			return apperrors.DepositNotApplicable()
		}
		if err != nil {
			return err
		}
		if inv.Status == "paid" {
			return apperrors.DepositAlreadyPaid()
		}
		if _, err := q.DeleteDepositLine(ctx, inv.ID); err != nil {
			return err
		}
		if err := q.WaiveDeposit(ctx, sqlcgen.WaiveDepositParams{
			ID: id, DepositWaivedBy: &userID, DepositWaiverReason: &reason,
		}); err != nil {
			return err
		}
		meta, _ := json.Marshal(map[string]any{"reason": reason, "amount": *b.DepositAmount})
		return q.InsertAuditLog(ctx, sqlcgen.InsertAuditLogParams{
			ID: uuid.Must(uuid.NewV7()), OwnerID: ownerID, ActorUserID: userID,
			Action: "booking.deposit.waived", Entity: "booking", EntityID: id, Metadata: meta,
		})
	})
}

func (s *Service) depositTx(ctx context.Context, id uuid.UUID,
	act func(q *sqlcgen.Queries, b sqlcgen.LockBookingDepositRow) error) (Booking, error) {
	var row sqlcgen.GetBookingRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		b, err := q.LockBookingDeposit(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFoundBooking()
		}
		if err != nil {
			return err
		}
		if err := act(q, b); err != nil {
			return err
		}
		row, err = q.GetBooking(ctx, id)
		return err
	})
	if err != nil {
		return Booking{}, translate(err, "deposit")
	}
	return bookingOf(row), nil
}

func returnCharges(ctx context.Context, q *sqlcgen.Queries, id uuid.UUID) ([]charge, error) {
	rows, err := q.ReturnCharges(ctx, &id)
	if err != nil {
		return nil, err
	}
	out := make([]charge, 0, len(rows))
	for _, r := range rows {
		out = append(out, charge{kind: r.Kind, description: r.Description, amount: r.Amount, photoID: r.HandoverPhotoID})
	}
	return out, nil
}

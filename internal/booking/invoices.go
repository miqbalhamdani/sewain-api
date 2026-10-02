package booking

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// Invoices of a booking.  (S1-041, pulled forward into M3)
//
// They live in this package because in phase 1 every invoice belongs to a
// booking -- subscriptions are deferred -- and every write of one happens
// inside a booking transaction: create issues the first, return the next.

type Invoice struct {
	ID        uuid.UUID
	BookingID uuid.UUID
	Number    string
	Status    string
	DueAt     time.Time
	PaidAt    *time.Time
	CreatedAt time.Time
	Total     int64
	Lines     []InvoiceLine
}

type InvoiceLine struct {
	ID              uuid.UUID
	Kind            string
	Description     string
	Amount          int64
	HandoverPhotoID *uuid.UUID
	WaiverReason    *string
}

type line struct {
	kind, description string
	amount            int64
	photoID           *uuid.UUID
	waiverReason      *string
}

// issueInvoice writes one invoice and its lines, or nothing when there are no
// lines: an invoice of nothing is not a bill. Numbered "<code>/<n>"; the
// caller holds the booking row (or just created it), so n cannot race.
func issueInvoice(ctx context.Context, q *sqlcgen.Queries, ownerID, userID, bookingID, customerID uuid.UUID,
	code string, due time.Time, lines []line) error {
	if len(lines) == 0 {
		return nil
	}
	n, err := q.CountBookingInvoices(ctx, &bookingID)
	if err != nil {
		return err
	}
	invoiceID := uuid.Must(uuid.NewV7())
	if err := q.InsertInvoice(ctx, sqlcgen.InsertInvoiceParams{
		ID: invoiceID, OwnerID: ownerID, BookingID: &bookingID, CustomerID: &customerID,
		Number: fmt.Sprintf("%s/%d", code, n+1), DueAt: due, CreatedBy: &userID,
	}); err != nil {
		return err
	}
	for _, l := range lines {
		if err := q.InsertInvoiceLine(ctx, sqlcgen.InsertInvoiceLineParams{
			ID: uuid.Must(uuid.NewV7()), OwnerID: ownerID, InvoiceID: invoiceID,
			Kind: l.kind, Description: l.description, Amount: l.amount,
			HandoverPhotoID: l.photoID, WaiverReason: l.waiverReason,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Invoices lists a booking's invoices with their lines. Total is the sum of
// the lines, computed by the query -- there is no total column (BR-055).
func (s *Service) Invoices(ctx context.Context, bookingID uuid.UUID) ([]Invoice, error) {
	var out []Invoice
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		rows, err := q.ListBookingInvoices(ctx, &bookingID)
		if err != nil {
			return err
		}
		out = make([]Invoice, 0, len(rows))
		for _, r := range rows {
			out = append(out, invoiceOf(sqlcgen.GetInvoiceRow(r)))
		}
		return attachLines(ctx, q, out)
	})
	if err != nil {
		return nil, fmt.Errorf("list invoices: %w", err)
	}
	return out, nil
}

func (s *Service) Invoice(ctx context.Context, id uuid.UUID) (Invoice, error) {
	var inv []Invoice
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		row, err := q.GetInvoice(ctx, id)
		if err != nil {
			return err
		}
		inv = []Invoice{invoiceOf(row)}
		return attachLines(ctx, q, inv)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Invoice{}, apperrors.NotFound("No such invoice in this business.")
	}
	if err != nil {
		return Invoice{}, fmt.Errorf("get invoice: %w", err)
	}
	return inv[0], nil
}

func attachLines(ctx context.Context, q *sqlcgen.Queries, invoices []Invoice) error {
	if len(invoices) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(invoices))
	at := make(map[uuid.UUID]int, len(invoices))
	for i, inv := range invoices {
		ids[i], at[inv.ID] = inv.ID, i
	}
	rows, err := q.ListInvoiceLines(ctx, ids)
	if err != nil {
		return err
	}
	for i := range invoices {
		invoices[i].Lines = []InvoiceLine{}
	}
	for _, r := range rows {
		i := at[r.InvoiceID]
		invoices[i].Lines = append(invoices[i].Lines, InvoiceLine{
			ID: r.ID, Kind: r.Kind, Description: r.Description, Amount: r.Amount,
			HandoverPhotoID: r.HandoverPhotoID, WaiverReason: r.WaiverReason,
		})
	}
	return nil
}

func invoiceOf(r sqlcgen.GetInvoiceRow) Invoice {
	var booking uuid.UUID
	if r.BookingID != nil {
		booking = *r.BookingID
	}
	return Invoice{ID: r.ID, BookingID: booking, Number: r.Number, Status: r.Status,
		DueAt: r.DueAt, PaidAt: r.PaidAt, CreatedAt: r.CreatedAt, Total: r.Total}
}

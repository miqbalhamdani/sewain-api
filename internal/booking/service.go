// Package booking holds bookings, availability and the calendar.
// (S1-022 .. S1-027, S1-078)
//
// The claim this package exists to keep is "no double booking", and it does
// not keep it. bookings_no_overlap does (BR-022). Everything here that looks
// like a conflict check -- FindConflicts before a write -- is only there so
// the refusal comes with the booking that caused it. Losing a race past that
// check lands on the constraint, and translate turns the constraint's refusal
// into the same booking-conflict. S1-023 is the test that keeps it that way:
// delete the constraint and it goes red, whatever this package checks.
package booking

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
	"github.com/miqbalhamdani/sewain-api/internal/storage"
)

type Service struct {
	store   *db.Store
	objects Objects
	jobs    Enqueuer
	scanner Scanner
}

// Objects is what handovers need from object storage (internal/storage),
// declared here by the consumer.
type Objects interface {
	Promote(ctx context.Context, ownerID uuid.UUID, pendingKey, finalPrefix, field string,
		allowed map[string]bool) (storage.Object, string, error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

func New(store *db.Store, objects Objects) *Service { return &Service{store: store, objects: objects} }

// Booking is one booking as every backoffice screen reads it.
type Booking struct {
	ID              uuid.UUID
	Code            string
	Status          string
	Source          string
	StartAt         time.Time
	EndAt           time.Time
	EndAtWithBuffer time.Time
	Overdue         bool

	// Ringkasan bayar, turunan baca seperti Overdue -- lihat BookingPayment di kontrak.
	PaymentStatus string
	Outstanding   int64

	UnitPrice      int64
	PricingUnit    string
	BufferMinutes  int32
	DurationQty    int32
	Subtotal       int64
	DepositAmount  *int64
	LateFeePerUnit *int64

	CancelledReason *string
	ExpiresAt       *time.Time
	CreatedAt       time.Time
	ActualReturnAt  *time.Time

	DepositWaivedAt  *time.Time
	DepositSettledAt *time.Time
	DepositDeducted  int64
	DepositRefunded  int64

	CustomerID          uuid.UUID
	CustomerName        string
	CustomerPhone       string
	CustomerBlacklisted bool
	ResourceID          uuid.UUID
	ResourceName        string
	UnitID              uuid.UUID
	UnitCode            string
	UnitLabel           *string
}

// NewBooking is what a staff create carries. Everything else is the server's:
// code, resource, every snapshot (BR-014, BR-015, BR-024).
type NewBooking struct {
	CustomerID uuid.UUID
	UnitID     uuid.UUID
	StartAt    time.Time
	EndAt      time.Time
}

// Filter narrows a list. Overdue is a derived condition (BR-041), not a status.
type Filter struct {
	Status   *string
	From, To *time.Time
	// Empty means "any". A nil slice reaches the query as NULL.
	UnitIDs     []uuid.UUID
	CustomerIDs []uuid.UUID
	ResourceIDs []uuid.UUID
	// Substring match; nil or blank means "any".
	Code    *string
	Overdue bool
}

// Cursor is the keyset position after the last row of a page.
type Cursor struct {
	StartAt time.Time
	ID      uuid.UUID
}

func notFoundBooking() error  { return apperrors.NotFound("No such booking in this business.") }
func notFoundUnit() error     { return apperrors.NotFound("No such unit in this business.") }
func notFoundCustomer() error { return apperrors.NotFound("No such customer in this business.") }

// errOverlap marks a write refused by bookings_no_overlap. It never reaches a
// client: the caller re-reads the conflicts in a fresh transaction -- the one
// that failed is aborted -- and answers BookingConflict.
var errOverlap = errors.New("bookings_no_overlap refused the write")

// Create makes a staff booking, which is born reserved (04-api-spec.md 3.5).
func (s *Service) Create(ctx context.Context, userID uuid.UUID, n NewBooking) (Booking, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return Booking{}, db.ErrNoOwnerContext
	}
	if !n.EndAt.After(n.StartAt) {
		return Booking{}, apperrors.ValidationFailed("end_at must be after start_at.").
			WithFields(apperrors.Field{Name: "end_at"})
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Booking{}, fmt.Errorf("new booking id: %w", err)
	}

	var row sqlcgen.GetBookingRow
	var buffer int32
	err = s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)

		unit, err := q.GetBookableUnit(ctx, n.UnitID)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFoundUnit()
		}
		if err != nil {
			return err
		}
		buffer = unit.BufferMinutes
		if unit.UnitStatus != "active" || unit.ResourceStatus != "active" {
			// BR-013: only active units are offered. Maintenance keeps its
			// existing bookings but takes no new ones.
			return apperrors.ValidationFailed("That unit is not available for new bookings.").
				WithFields(apperrors.Field{Name: "resource_unit_id"})
		}

		blacklisted, err := q.GetCustomerBlacklisted(ctx, n.CustomerID)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFoundCustomer()
		}
		if err != nil {
			return err
		}
		if blacklisted {
			return apperrors.CustomerBlacklisted()
		}

		qty, err := durationQty(n.StartAt, n.EndAt, unit.PricingUnit)
		if err != nil {
			return err
		}
		if err := checkDuration(qty, unit.MinDuration, unit.MaxDuration, unit.PricingUnit); err != nil {
			return err
		}

		if err := precheck(ctx, q, n.UnitID, uuid.Nil, n.StartAt, n.EndAt, buffer); err != nil {
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

		if err := q.InsertBooking(ctx, sqlcgen.InsertBookingParams{
			ID: id, OwnerID: ownerID, CreatedBy: &userID,
			Code:       fmt.Sprintf("%s-%04d", prefix, number),
			CustomerID: n.CustomerID, ResourceID: unit.ResourceID, ResourceUnitID: n.UnitID,
			StartAt: n.StartAt, EndAt: n.EndAt,
			Status: "reserved", Source: "staff",
			UnitPrice: unit.BasePrice, PricingUnit: unit.PricingUnit,
			BufferMinutes: unit.BufferMinutes, DurationQty: qty,
			Subtotal:       int64(qty) * unit.BasePrice,
			DepositAmount:  unit.DepositAmount,
			LateFeePerUnit: unit.LateFeePerUnit,
		}); err != nil {
			return err
		}

		// BR-045 + BR-057: the first invoice is issued with the booking, in the
		// same transaction -- rent, plus the deposit when the resource takes one.
		if err := issueFirstInvoice(ctx, q, ownerID, userID, id, n.CustomerID,
			fmt.Sprintf("%s-%04d", prefix, number), n.StartAt, unit.PricingUnit, qty,
			int64(qty)*unit.BasePrice, unit.DepositAmount); err != nil {
			return err
		}
		row, err = q.GetBooking(ctx, id)
		return err
	})
	if err != nil {
		return Booking{}, s.answer(ctx, err, "create booking", n.UnitID, uuid.Nil, n.StartAt, n.EndAt, buffer)
	}
	return bookingOf(row), nil
}

// issueFirstInvoice is the invoice a booking is born with -- rent, plus the
// deposit when the resource takes one -- due at min(now + payment_due_hours,
// start_at) (BR-045, BR-057). Create calls it for staff bookings, which are
// born reserved; Confirm calls it when a public draft becomes reserved, because
// that is the moment the deadline starts ticking (PRD 7.5). A draft never has
// an invoice.
func issueFirstInvoice(ctx context.Context, q *sqlcgen.Queries, ownerID, userID, bookingID, customerID uuid.UUID,
	code string, startAt time.Time, pricingUnit string, qty int32, subtotal int64, deposit *int64) error {
	dueHours, err := q.GetPaymentDueHours(ctx, ownerID)
	if err != nil {
		return err
	}
	due := time.Now().Add(time.Duration(dueHours) * time.Hour)
	if startAt.Before(due) {
		due = startAt
	}
	var lines []line
	if subtotal > 0 {
		lines = append(lines, line{kind: "rent", amount: subtotal,
			description: fmt.Sprintf("Sewa %d %s", qty, unitName[pricingUnit])})
	}
	if deposit != nil {
		lines = append(lines, line{kind: "deposit", amount: *deposit, description: "Deposit"})
	}
	return issueInvoice(ctx, q, ownerID, userID, bookingID, customerID, code, due, lines)
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Booking, error) {
	var row sqlcgen.GetBookingRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).GetBooking(ctx, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Booking{}, notFoundBooking()
	}
	if err != nil {
		return Booking{}, fmt.Errorf("get booking: %w", err)
	}
	return bookingOf(row), nil
}

// List returns one page, latest start first, and the cursor after it (nil on
// the last page).
func (s *Service) List(ctx context.Context, f Filter, after *Cursor, limit int) ([]Booking, *Cursor, error) {
	params := sqlcgen.ListBookingsParams{
		Status: f.Status, FromAt: f.From, ToAt: f.To, UnitIds: f.UnitIDs,
		CustomerIds: f.CustomerIDs, ResourceIds: f.ResourceIDs,
		Code: f.Code, OverdueOnly: f.Overdue,
		Lim: int32(limit + 1), //nolint:gosec // bounded by the schema's maximum
	}
	if after != nil {
		params.CursorStart, params.CursorID = &after.StartAt, &after.ID
	}
	var rows []sqlcgen.ListBookingsRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlcgen.New(tx).ListBookings(ctx, params)
		return err
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list bookings: %w", err)
	}
	var next *Cursor
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		next = &Cursor{StartAt: last.StartAt, ID: last.ID}
	}
	out := make([]Booking, 0, len(rows))
	for _, r := range rows {
		out = append(out, bookingOf(sqlcgen.GetBookingRow(r)))
	}
	return out, next, nil
}

// Confirm turns a draft into reserved, re-running the conflict check at that
// moment: a draft never held the unit, so this can fail, and that is correct
// (BR-026). The first invoice is issued in the same transaction: reserved is
// where due_at starts ticking (PRD 7.5, BR-057), and a draft never had one.
func (s *Service) Confirm(ctx context.Context, userID, id uuid.UUID) (Booking, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return Booking{}, db.ErrNoOwnerContext
	}
	var locked sqlcgen.LockBookingRow
	return s.transition(ctx, id, &locked, func(q *sqlcgen.Queries) error {
		if locked.Status != "draft" {
			return wrongStatus("Only a draft can be confirmed; this booking is " + locked.Status + ".")
		}
		if err := precheck(ctx, q, locked.ResourceUnitID, id, locked.StartAt, locked.EndAt, locked.BufferMinutes); err != nil {
			return err
		}
		if err := q.ConfirmBooking(ctx, id); err != nil {
			return err
		}
		return issueFirstInvoice(ctx, q, ownerID, userID, id, locked.CustomerID, locked.Code,
			locked.StartAt, locked.PricingUnit, locked.DurationQty, locked.Subtotal, locked.DepositAmount)
	})
}

// Cancel releases the unit at once: bookings_no_overlap stops seeing the row
// (BR-023). Only draft and reserved can be called off; anything later has a
// handover behind it.
func (s *Service) Cancel(ctx context.Context, id uuid.UUID) (Booking, error) {
	var locked sqlcgen.LockBookingRow
	return s.transition(ctx, id, &locked, func(q *sqlcgen.Queries) error {
		if locked.Status != "draft" && locked.Status != "reserved" {
			return wrongStatus("Only a draft or reserved booking can be cancelled; this booking is " + locked.Status + ".")
		}
		if err := q.CancelBooking(ctx, id); err != nil {
			return err
		}
		// Booking batal tidak menagih apa pun (BR-057 revisi): invoice yang
		// belum dibayar ikut batal. S1-052 memakai jalur yang sama nanti.
		return q.CancelBookingInvoices(ctx, &id)
	})
}

// Swap moves a reserved booking to another unit of the same resource (BR-029).
// "Same resource" is the database's to say -- bookings_unit_matches_resource --
// and the price snapshot is not re-read: the unit changes, the kind does not.
func (s *Service) Swap(ctx context.Context, id, unitID uuid.UUID) (Booking, error) {
	var locked sqlcgen.LockBookingRow
	return s.transition(ctx, id, &locked, func(q *sqlcgen.Queries) error {
		if locked.Status != "reserved" {
			return apperrors.UnitNotSwappable(locked.Status)
		}
		if locked.ResourceUnitID == unitID {
			return nil
		}
		target, err := q.GetBookableUnit(ctx, unitID)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFoundUnit()
		}
		if err != nil {
			return err
		}
		if target.UnitStatus != "active" {
			return apperrors.ValidationFailed("That unit is not available for new bookings.").
				WithFields(apperrors.Field{Name: "resource_unit_id"})
		}
		if err := precheck(ctx, q, unitID, id, locked.StartAt, locked.EndAt, locked.BufferMinutes); err != nil {
			return err
		}
		return q.SwapBookingUnit(ctx, sqlcgen.SwapBookingUnitParams{ID: id, UnitID: unitID})
	})
}

// transition is the shape every action endpoint shares: lock the row, decide
// from its status, write, read back. The lock is what keeps a confirm and a
// cancel on the same booking from both deciding off the same old status.
func (s *Service) transition(ctx context.Context, id uuid.UUID, locked *sqlcgen.LockBookingRow,
	act func(q *sqlcgen.Queries) error) (Booking, error) {
	var row sqlcgen.GetBookingRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var err error
		if *locked, err = q.LockBooking(ctx, id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return notFoundBooking()
			}
			return err
		}
		if err := act(q); err != nil {
			return err
		}
		row, err = q.GetBooking(ctx, id)
		return err
	})
	if err != nil {
		return Booking{}, s.answer(ctx, err, "booking transition", locked.ResourceUnitID, id,
			locked.StartAt, locked.EndAt, locked.BufferMinutes)
	}
	return bookingOf(row), nil
}

// precheck finds what would conflict before writing, purely so the refusal can
// name it. It is not what prevents the double booking -- see the package doc.
func precheck(ctx context.Context, q *sqlcgen.Queries, unitID, excludeID uuid.UUID,
	start, end time.Time, buffer int32) error {
	rows, err := q.FindConflicts(ctx, sqlcgen.FindConflictsParams{
		UnitID: unitID, ExcludeID: excludeID, StartAt: start, EndAt: end, BufferMinutes: buffer,
	})
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		return apperrors.BookingConflict(conflictsOf(rows))
	}
	return nil
}

// answer turns whatever a write returned into what the client sees. Losing the
// race to the constraint becomes the same booking-conflict the pre-check gives,
// with the winner read in a fresh transaction.
func (s *Service) answer(ctx context.Context, err error, verb string, unitID, excludeID uuid.UUID,
	start, end time.Time, buffer int32) error {
	err = translate(err, verb)
	if !errors.Is(err, errOverlap) {
		return err
	}
	var rows []sqlcgen.FindConflictsRow
	if readErr := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var e error
		rows, e = sqlcgen.New(tx).FindConflicts(ctx, sqlcgen.FindConflictsParams{
			UnitID: unitID, ExcludeID: excludeID, StartAt: start, EndAt: end, BufferMinutes: buffer,
		})
		return e
	}); readErr != nil {
		// The refusal is still the answer; only its list is missing.
		return apperrors.BookingConflict(nil).WithCause(err)
	}
	return apperrors.BookingConflict(conflictsOf(rows)).WithCause(err)
}

func translate(err error, verb string) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		var known *apperrors.Error
		if errors.As(err, &known) {
			return known
		}
		return fmt.Errorf("%s: %w", verb, err)
	}
	switch pgErr.ConstraintName {
	case "bookings_no_overlap":
		return fmt.Errorf("%w: %w", errOverlap, err)
	case "bookings_unit_matches_resource":
		return apperrors.ValidationFailed(
			"A booking can only move to another unit of the same resource.").
			WithFields(apperrors.Field{Name: "resource_unit_id"}).WithCause(err)
	case "bookings_customer_matches_owner":
		return notFoundCustomer()
	case "bookings_range_valid":
		return apperrors.ValidationFailed("end_at must be after start_at.").
			WithFields(apperrors.Field{Name: "end_at"}).WithCause(err)
	}
	return fmt.Errorf("%s: %w", verb, err)
}

func wrongStatus(detail string) error {
	return apperrors.ValidationFailed(detail).WithFields(apperrors.Field{Name: "status"})
}

func conflictsOf(rows []sqlcgen.FindConflictsRow) []apperrors.Conflict {
	out := make([]apperrors.Conflict, 0, len(rows))
	for _, r := range rows {
		out = append(out, apperrors.Conflict{Code: r.Code, StartAt: r.StartAt, EndAt: r.EndAt, Status: r.Status})
	}
	return out
}

// paymentOf menurunkan badge bayar satu booking (ide booking-invoice-lists).
// Invoice cancelled sudah disingkirkan query-nya; gateway_pending terhitung
// belum bayar -- uang yang belum terkonfirmasi bukan uang.
func paymentOf(bookingStatus string, nActive, nOverdue, nUnpaid int32, outstanding int64) (string, int64) {
	switch bookingStatus {
	case "draft", "cancelled", "no_show":
		return "none", 0
	}
	switch {
	case nActive == 0:
		return "none", 0
	case nOverdue > 0:
		return "overdue", outstanding
	case nUnpaid > 0:
		return "unpaid", outstanding
	default:
		return "paid", 0
	}
}

func bookingOf(r sqlcgen.GetBookingRow) Booking {
	pay, outstanding := paymentOf(r.Status, r.NActive, r.NOverdue, r.NUnpaid, r.Outstanding)
	return Booking{
		ID: r.ID, Code: r.Code, Status: r.Status, Source: r.Source,
		StartAt: r.StartAt, EndAt: r.EndAt, EndAtWithBuffer: r.EndAtWithBuffer, Overdue: r.Overdue,
		PaymentStatus: pay, Outstanding: outstanding,
		UnitPrice: r.UnitPrice, PricingUnit: r.PricingUnit, BufferMinutes: r.BufferMinutes,
		DurationQty: r.DurationQty, Subtotal: r.Subtotal,
		DepositAmount: r.DepositAmount, LateFeePerUnit: r.LateFeePerUnit,
		CancelledReason: r.CancelledReason, ExpiresAt: r.ExpiresAt, CreatedAt: r.CreatedAt,
		ActualReturnAt:  r.ActualReturnAt,
		DepositWaivedAt: r.DepositWaivedAt, DepositSettledAt: r.DepositSettledAt,
		DepositDeducted: r.DepositDeducted, DepositRefunded: r.DepositRefunded,
		CustomerID: r.CustomerID, CustomerName: r.CustomerName, CustomerPhone: r.CustomerPhone,
		CustomerBlacklisted: r.CustomerBlacklisted,
		ResourceID:          r.ResourceID, ResourceName: r.ResourceName,
		UnitID: r.UnitID, UnitCode: r.UnitCode, UnitLabel: r.UnitLabel,
	}
}

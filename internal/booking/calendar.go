package booking

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
)

// The eight states of BR-033. retired is in the vocabulary but never emitted:
// a retired unit has no lane at all.
const (
	StateAvailable      = "available"
	StateReservedUnpaid = "reserved_unpaid"
	StateReservedPaid   = "reserved_paid"
	StatePickedUp       = "picked_up"
	StateOverdue        = "overdue"
	StateBuffer         = "buffer"
	StateMaintenance    = "maintenance"
)

// CalendarRow is one unit's lane.
type CalendarRow struct {
	Unit       UnitRef
	ResourceID uuid.UUID
	Segments   []Segment
}

// Segment is a stretch of one state. Segments cover [from, to) with no gaps.
type Segment struct {
	From, To time.Time
	State    string
	Booking  *SegmentBooking
}

type SegmentBooking struct {
	ID           uuid.UUID
	Code         string
	CustomerName string
}

type calBooking struct {
	SegmentBooking
	StartAt, EndAt, EndAtWithBuffer time.Time
	Status                          string
}

// Calendar computes every lane on the server (BR-033): the client never
// assembles a state from booking status, invoice status and unit status, which
// is where two screens start to disagree.
func (s *Service) Calendar(ctx context.Context, from, to, now time.Time) ([]CalendarRow, error) {
	if err := checkRange(from, to, "to"); err != nil {
		return nil, err
	}
	var units []sqlcgen.ListCalendarUnitsRow
	var bookings []sqlcgen.ListCalendarBookingsRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		var err error
		if units, err = q.ListCalendarUnits(ctx); err != nil {
			return err
		}
		bookings, err = q.ListCalendarBookings(ctx, sqlcgen.ListCalendarBookingsParams{FromAt: from, ToAt: to})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("calendar: %w", err)
	}

	byUnit := make(map[uuid.UUID][]calBooking, len(units))
	for _, b := range bookings {
		byUnit[b.ResourceUnitID] = append(byUnit[b.ResourceUnitID], calBooking{
			SegmentBooking:  SegmentBooking{ID: b.ID, Code: b.Code, CustomerName: b.CustomerName},
			StartAt:         b.StartAt,
			EndAt:           b.EndAt,
			EndAtWithBuffer: b.EndAtWithBuffer,
			Status:          b.Status,
		})
	}
	out := make([]CalendarRow, 0, len(units))
	for _, u := range units {
		out = append(out, CalendarRow{
			Unit:       UnitRef{ID: u.ID, Code: u.Code, Label: u.Label},
			ResourceID: u.ResourceID,
			Segments:   segments(from, to, u.Status, byUnit[u.ID], now),
		})
	}
	return out, nil
}

// segments lays one unit's bookings over [from, to). The bookings arrive sorted
// by start and, thanks to bookings_no_overlap, never overlap each other --
// buffer included -- so a single forward walk is enough.
//
// reserved is always reserved_unpaid until S1-041 brings invoices: nothing has
// been paid yet, so that is the true state, not a placeholder.
func segments(from, to time.Time, unitStatus string, bookings []calBooking, now time.Time) []Segment {
	if unitStatus == "maintenance" {
		return []Segment{{From: from, To: to, State: StateMaintenance}}
	}
	out := []Segment{}
	add := func(a, b time.Time, state string, bk *SegmentBooking) {
		if a.Before(from) {
			a = from
		}
		if b.After(to) {
			b = to
		}
		if a.Before(b) {
			out = append(out, Segment{From: a, To: b, State: state, Booking: bk})
		}
	}
	cursor := from
	for i := range bookings {
		b := bookings[i]
		add(cursor, b.StartAt, StateAvailable, nil)
		state := StateReservedUnpaid
		if b.Status == "picked_up" {
			state = StatePickedUp
			if b.EndAt.Before(now) {
				state = StateOverdue // BR-041: derived, never stored
			}
		}
		bk := b.SegmentBooking
		add(b.StartAt, b.EndAt, state, &bk)
		add(b.EndAt, b.EndAtWithBuffer, StateBuffer, nil)
		if b.EndAtWithBuffer.After(cursor) {
			cursor = b.EndAtWithBuffer
		}
	}
	add(cursor, to, StateAvailable, nil)
	return out
}

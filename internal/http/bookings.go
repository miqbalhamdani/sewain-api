package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	"github.com/miqbalhamdani/sewain-api/internal/booking"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// Bookings, availability and the calendar over HTTP.
// (S1-025 .. S1-027, S1-078)

// ListBookings handles GET /bookings.
func (s *Server) ListBookings(w http.ResponseWriter, r *http.Request, params ListBookingsParams) {
	requirePermission(auth.PermBookingsRead, func(w http.ResponseWriter, r *http.Request) {
		var after *booking.Cursor
		if params.Cursor != nil {
			c, ok := decodeBookingCursor(*params.Cursor)
			if !ok {
				writeError(w, r, badCursor())
				return
			}
			after = &c
		}
		rows, next, err := s.bookings.List(r.Context(), booking.Filter{
			Status: stringPtr(params.Status), From: params.From, To: params.To,
			UnitIDs: uuids(params.UnitId), CustomerIDs: uuids(params.CustomerId), ResourceIDs: uuids(params.ResourceId),
			Code:    nonBlank(params.Code),
			Overdue: params.Overdue != nil && *params.Overdue,
		}, after, pageLimit(params.Limit))
		if err != nil {
			writeError(w, r, err)
			return
		}
		page := BookingPage{Data: make([]Booking, 0, len(rows))}
		for _, b := range rows {
			page.Data = append(page.Data, bookingBody(b))
		}
		if next != nil {
			page.NextCursor = ptr(encodeBookingCursor(*next))
		}
		writeJSON(w, r, http.StatusOK, page)
	})(w, r)
}

// CreateBooking handles POST /bookings. Idempotency-Key is enforced by the
// middleware, before this runs (BR-090).
func (s *Server) CreateBooking(w http.ResponseWriter, r *http.Request, _ CreateBookingParams) {
	requirePermission(auth.PermBookingsWrite, func(w http.ResponseWriter, r *http.Request) {
		raw, present, ok := readBody(w, r)
		if !ok {
			return
		}
		// The price is the server's, from the resource at this moment (BR-014).
		// end_at_with_buffer is NOT here: a client value for it is overwritten
		// by the trigger, not refused (04-api-spec.md 3.5).
		for _, field := range []string{"id", "owner_id", "code", "status", "resource_id",
			"unit_price", "pricing_unit", "deposit_amount", "late_fee_per_unit",
			"buffer_minutes", "duration_qty", "subtotal", "actual_return_at"} {
			if _, sent := present[field]; sent {
				writeError(w, r, apperrors.ValidationFailed(
					field+" is set by the server and cannot be sent.").
					WithFields(apperrors.Field{Name: field}))
				return
			}
		}
		var body BookingCreate
		if err := json.Unmarshal(raw, &body); err != nil {
			writeError(w, r, malformed(err))
			return
		}
		userID, ok := auth.UserFromContext(r.Context())
		if !ok {
			writeError(w, r, apperrors.Unauthenticated("A bearer token is required."))
			return
		}
		b, err := s.bookings.Create(r.Context(), userID, booking.NewBooking{
			CustomerID: body.CustomerId, UnitID: body.ResourceUnitId,
			StartAt: body.StartAt, EndAt: body.EndAt,
		})
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusCreated, bookingBody(b))
	})(w, r)
}

// GetBooking handles GET /bookings/{id}.
func (s *Server) GetBooking(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermBookingsRead, func(w http.ResponseWriter, r *http.Request) {
		b, err := s.bookings.Get(r.Context(), id)
		s.writeBooking(w, r, b, err)
	})(w, r)
}

// UpdateBooking handles PATCH /bookings/{id}: swap unit, and nothing else in
// phase 1 (BR-029). There is no PATCH {status}; transitions are actions.
func (s *Server) UpdateBooking(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermBookingsWrite, func(w http.ResponseWriter, r *http.Request) {
		raw, present, ok := readBody(w, r)
		if !ok {
			return
		}
		for field := range present {
			if field != "resource_unit_id" {
				writeError(w, r, apperrors.ValidationFailed(
					field+" cannot be changed here; only resource_unit_id can.").
					WithFields(apperrors.Field{Name: field}))
				return
			}
		}
		var body BookingUpdate
		if err := json.Unmarshal(raw, &body); err != nil || body.ResourceUnitId == uuid.Nil {
			writeError(w, r, apperrors.ValidationFailed("resource_unit_id is required.").
				WithFields(apperrors.Field{Name: "resource_unit_id"}))
			return
		}
		b, err := s.bookings.Swap(r.Context(), id, body.ResourceUnitId)
		s.writeBooking(w, r, b, err)
	})(w, r)
}

// ConfirmBooking handles POST /bookings/{id}/confirm.
func (s *Server) ConfirmBooking(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermBookingsWrite, func(w http.ResponseWriter, r *http.Request) {
		b, err := s.bookings.Confirm(r.Context(), id)
		s.writeBooking(w, r, b, err)
	})(w, r)
}

// CancelBooking handles POST /bookings/{id}/cancel.
func (s *Server) CancelBooking(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermBookingsWrite, func(w http.ResponseWriter, r *http.Request) {
		b, err := s.bookings.Cancel(r.Context(), id)
		s.writeBooking(w, r, b, err)
	})(w, r)
}

func (s *Server) writeBooking(w http.ResponseWriter, r *http.Request, b booking.Booking, err error) {
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, bookingBody(b))
}

// GetAvailability handles GET /availability.
func (s *Server) GetAvailability(w http.ResponseWriter, r *http.Request, params GetAvailabilityParams) {
	requirePermission(auth.PermBookingsRead, func(w http.ResponseWriter, r *http.Request) {
		rows, err := s.bookings.Availability(r.Context(), params.StartAt, params.EndAt, params.ResourceId)
		if err != nil {
			writeError(w, r, err)
			return
		}
		data := make([]AvailabilityResult, 0, len(rows))
		for _, a := range rows {
			res := AvailabilityResult{
				AvailableUnits: unitRefs(a.Units),
				DurationQty:    int(a.DurationQty),
				Subtotal:       a.Subtotal,
			}
			res.Resource.Id, res.Resource.Name = a.ResourceID, a.ResourceName
			res.Resource.BasePrice, res.Resource.PricingUnit = a.BasePrice, PricingUnit(a.PricingUnit)
			res.Resource.DepositAmount, res.Resource.BufferMinutes = a.DepositAmount, int(a.BufferMinutes)
			data = append(data, res)
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"data": data})
	})(w, r)
}

// GetCalendar handles GET /calendar. Every state is the server's (BR-033).
func (s *Server) GetCalendar(w http.ResponseWriter, r *http.Request, params GetCalendarParams) {
	requirePermission(auth.PermBookingsRead, func(w http.ResponseWriter, r *http.Request) {
		rows, err := s.bookings.Calendar(r.Context(), params.From, params.To, time.Now())
		if err != nil {
			writeError(w, r, err)
			return
		}
		data := make([]CalendarRow, 0, len(rows))
		for _, row := range rows {
			segs := make([]CalendarSegment, 0, len(row.Segments))
			for _, sg := range row.Segments {
				seg := CalendarSegment{From: sg.From, To: sg.To, State: CalendarState(sg.State)}
				if sg.Booking != nil {
					seg.Booking = &struct {
						Code         string    `json:"code"`
						CustomerName string    `json:"customer_name"`
						Id           uuid.UUID `json:"id"`
					}{Code: sg.Booking.Code, CustomerName: sg.Booking.CustomerName, Id: sg.Booking.ID}
				}
				segs = append(segs, seg)
			}
			data = append(data, CalendarRow{
				Unit:       UnitRef{Id: row.Unit.ID, Code: row.Unit.Code, Label: row.Unit.Label},
				ResourceId: row.ResourceID,
				Segments:   segs,
			})
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"data": data})
	})(w, r)
}

func bookingBody(b booking.Booking) Booking {
	return Booking{
		Id: b.ID, Code: b.Code, Status: BookingStatus(b.Status), Source: BookingSource(b.Source),
		Customer: BookingCustomer{Id: b.CustomerID, Name: b.CustomerName, Phone: b.CustomerPhone,
			IsBlacklisted: b.CustomerBlacklisted},
		Resource: BookingResource{Id: b.ResourceID, Name: b.ResourceName},
		Unit:     UnitRef{Id: b.UnitID, Code: b.UnitCode, Label: b.UnitLabel},
		StartAt:  b.StartAt, EndAt: b.EndAt, EndAtWithBuffer: b.EndAtWithBuffer, Overdue: b.Overdue,
		UnitPrice: b.UnitPrice, PricingUnit: PricingUnit(b.PricingUnit),
		BufferMinutes: int(b.BufferMinutes), DurationQty: int(b.DurationQty), Subtotal: b.Subtotal,
		DepositAmount: b.DepositAmount, LateFeePerUnit: b.LateFeePerUnit,
		CancelledReason: (*CancelledReason)(b.CancelledReason), ExpiresAt: b.ExpiresAt,
		CreatedAt: b.CreatedAt, ActualReturnAt: b.ActualReturnAt,
		DepositWaivedAt: b.DepositWaivedAt, DepositSettledAt: b.DepositSettledAt,
		DepositDeducted: b.DepositDeducted, DepositRefunded: b.DepositRefunded,
	}
}

func unitRefs(units []booking.UnitRef) []UnitRef {
	out := make([]UnitRef, 0, len(units))
	for _, u := range units {
		out = append(out, UnitRef{Id: u.ID, Code: u.Code, Label: u.Label})
	}
	return out
}

// The booking cursor is opaque to clients: base64 of "<start_at>|<id>".
func encodeBookingCursor(c booking.Cursor) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(c.StartAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String()))
}

func decodeBookingCursor(s string) (booking.Cursor, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return booking.Cursor{}, false
	}
	start, id, found := strings.Cut(string(raw), "|")
	if !found {
		return booking.Cursor{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, start)
	if err != nil {
		return booking.Cursor{}, false
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return booking.Cursor{}, false
	}
	return booking.Cursor{StartAt: t, ID: u}, true
}

// uuids turns an absent or empty repeated query param into nil, which the
// query reads as "any" -- an empty array would match nothing.
func uuids(p *[]uuid.UUID) []uuid.UUID {
	if p == nil || len(*p) == 0 {
		return nil
	}
	return *p
}

// nonBlank drops a text filter that is only whitespace.
func nonBlank(p *string) *string {
	if p == nil {
		return nil
	}
	t := strings.TrimSpace(*p)
	if t == "" {
		return nil
	}
	return &t
}

package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	"github.com/miqbalhamdani/sewain-api/internal/booking"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
	"github.com/miqbalhamdani/sewain-api/internal/storage"
)

// Uploads, handovers and invoices over HTTP.  (S1-033 .. S1-036, S1-041)

// PresignUpload handles POST /uploads/presign. Every role uploads -- an
// operator does every handover -- so the gate is the rate limit, not a role.
func (s *Server) PresignUpload(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserFromContext(r.Context())
	ownerID, ownerOK := owner.FromContext(r.Context())
	if !ok || !ownerOK {
		writeError(w, r, apperrors.Unauthenticated("A bearer token is required."))
		return
	}
	if !s.allow(w, r, "presign:"+userID.String(), 60, time.Minute) {
		return
	}
	var body PresignRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeError(w, r, malformed(err))
		return
	}
	allowed := storage.ImageTypes
	switch body.Kind {
	case UploadKindHandoverPhoto, UploadKindIdentityPhoto:
	case UploadKindPaymentProof:
		allowed = storage.ProofTypes // a bank e-statement is often a PDF (S1-046)
	default:
		writeError(w, r, apperrors.ValidationFailed("kind is not one this system accepts.").
			WithFields(apperrors.Field{Name: "kind"}))
		return
	}
	if !allowed[string(body.ContentType)] {
		writeError(w, r, apperrors.ValidationFailed("content_type is not allowed for this kind of upload.").
			WithFields(apperrors.Field{Name: "content_type"}))
		return
	}
	if body.Bytes < 1 || body.Bytes > storage.MaxUploadBytes {
		writeError(w, r, apperrors.ValidationFailed("A photo is at most 10 MB.").
			WithFields(apperrors.Field{Name: "bytes"}))
		return
	}
	key := storage.PendingKey(ownerID)
	url, err := s.objects.PresignPut(r.Context(), key, string(body.ContentType), body.Bytes)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, PresignedUpload{
		ObjectKey: key, UploadUrl: url, ExpiresIn: int(storage.UploadTTL.Seconds()),
		// Exactly what the signature binds; the browser sets Content-Length
		// itself from the file it sends.
		Headers: map[string]string{"Content-Type": string(body.ContentType)},
	})
}

// PickupBooking handles POST /bookings/{id}/pickup.
func (s *Server) PickupBooking(w http.ResponseWriter, r *http.Request, id uuid.UUID, _ PickupBookingParams) {
	requirePermission(auth.PermHandoversWrite, func(w http.ResponseWriter, r *http.Request) {
		var body PickupRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			writeError(w, r, malformed(err))
			return
		}
		userID, _ := auth.UserFromContext(r.Context())
		b, err := s.bookings.Pickup(r.Context(), userID, id, booking.PickupInput{
			HandoverInput:           handoverInput(body.PhotoKeys, body.MeterValue, body.Checklist, body.ConditionNotes),
			ConfirmPhysicalConflict: body.ConfirmPhysicalConflict != nil && *body.ConfirmPhysicalConflict,
		})
		s.writeBooking(w, r, b, err)
	})(w, r)
}

// PreviewReturn handles GET /bookings/{id}/return-preview.
func (s *Server) PreviewReturn(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermHandoversRead, func(w http.ResponseWriter, r *http.Request) {
		p, err := s.bookings.ReturnPreview(r.Context(), id, time.Now())
		if err != nil {
			writeError(w, r, err)
			return
		}
		body := ReturnPreview{
			ActualReturnAt: p.ActualReturnAt, EndAt: p.EndAt, OverdueUnits: int(p.OverdueUnits),
			PricingUnit: PricingUnit(p.PricingUnit), LateFeePerUnit: p.LateFeePerUnit,
			LateFeeTotal: p.LateFeeTotal, DepositAmount: p.DepositAmount,
		}
		body.ProposedLines = make([]struct {
			Amount      int64                          `json:"amount"`
			Description string                         `json:"description"`
			Kind        ReturnPreviewProposedLinesKind `json:"kind"`
		}, 0, 1)
		if p.LateFeeTotal > 0 {
			body.ProposedLines = append(body.ProposedLines, struct {
				Amount      int64                          `json:"amount"`
				Description string                         `json:"description"`
				Kind        ReturnPreviewProposedLinesKind `json:"kind"`
			}{Amount: p.LateFeeTotal, Description: booking.LateFeeDescription(p), Kind: ReturnPreviewProposedLinesKindLateFee})
		}
		writeJSON(w, r, http.StatusOK, body)
	})(w, r)
}

// ReturnBooking handles POST /bookings/{id}/return.
func (s *Server) ReturnBooking(w http.ResponseWriter, r *http.Request, id uuid.UUID, _ ReturnBookingParams) {
	requirePermission(auth.PermHandoversWrite, func(w http.ResponseWriter, r *http.Request) {
		raw, present, ok := readBody(w, r)
		if !ok {
			return
		}
		// BR-040: the return time is the server's clock. A body that tries to
		// set it is told so, not silently overruled.
		if _, sent := present["actual_return_at"]; sent {
			writeError(w, r, apperrors.ValidationFailed("actual_return_at is set by the server and cannot be sent.").
				WithFields(apperrors.Field{Name: "actual_return_at"}))
			return
		}
		var body ReturnRequest
		if err := json.Unmarshal(raw, &body); err != nil {
			writeError(w, r, malformed(err))
			return
		}
		in := booking.ReturnInput{
			HandoverInput:  handoverInput(body.PhotoKeys, body.MeterValue, body.Checklist, body.ConditionNotes),
			ConfirmLateFee: body.ConfirmLateFee != nil && *body.ConfirmLateFee,
		}
		if body.LateFeeWaived != nil {
			in.LateFeeWaived = *body.LateFeeWaived
		}
		if body.WaiverReason != nil {
			in.WaiverReason = *body.WaiverReason
		}
		if body.Damages != nil {
			for _, d := range *body.Damages {
				in.Damages = append(in.Damages, booking.Damage{Amount: d.Amount, Description: d.Description, PhotoKey: d.PhotoKey})
			}
		}
		userID, _ := auth.UserFromContext(r.Context())
		b, err := s.bookings.Return(r.Context(), userID, id, in)
		s.writeBooking(w, r, b, err)
	})(w, r)
}

// ListHandovers handles GET /bookings/{id}/handovers.
func (s *Server) ListHandovers(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermHandoversRead, func(w http.ResponseWriter, r *http.Request) {
		hs, err := s.bookings.Handovers(r.Context(), id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		out := make([]Handover, 0, len(hs))
		for _, h := range hs {
			body := Handover{
				Id: h.ID, Direction: HandoverDirection(h.Direction), PerformedBy: h.PerformedBy,
				PerformedAt: h.PerformedAt, MeterValue: h.MeterValue, Checklist: h.Checklist,
				ConditionNotes: h.ConditionNotes, LateFeeWaived: h.LateFeeWaived, WaiverReason: h.WaiverReason,
			}
			for _, p := range h.Photos {
				body.Photos = append(body.Photos, struct {
					CapturedAt time.Time `json:"captured_at"`
					Id         uuid.UUID `json:"id"`
					Url        string    `json:"url"`
				}{CapturedAt: p.CapturedAt, Id: p.ID, Url: p.URL})
			}
			out = append(out, body)
		}
		writeJSON(w, r, http.StatusOK, out)
	})(w, r)
}

// PatchHandover and DeleteHandover are registered only to answer 405 --
// evidence is append-only for every role, the owner included (BR-037).
func (s *Server) PatchHandover(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	writeError(w, r, apperrors.EvidenceImmutable())
}

func (s *Server) DeleteHandover(w http.ResponseWriter, r *http.Request, _ uuid.UUID) {
	writeError(w, r, apperrors.EvidenceImmutable())
}

// ListInvoices handles GET /invoices. With booking_id: that booking's invoices,
// every role. Without: the full list across bookings, owner only (reports:read,
// BR-003) -- an operator sees invoices through the booking they are serving.
func (s *Server) ListInvoices(w http.ResponseWriter, r *http.Request, params ListInvoicesParams) {
	perm := auth.PermInvoicesRead
	if params.BookingId == nil {
		perm = auth.PermReportsRead
	}
	requirePermission(perm, func(w http.ResponseWriter, r *http.Request) {
		page := InvoicePage{Data: []Invoice{}}
		if params.BookingId != nil {
			invs, err := s.bookings.Invoices(r.Context(), *params.BookingId)
			if err != nil {
				writeError(w, r, err)
				return
			}
			for _, inv := range invs {
				page.Data = append(page.Data, invoiceBody(inv))
			}
			writeJSON(w, r, http.StatusOK, page)
			return
		}
		var after *booking.InvoiceCursor
		if params.Cursor != nil {
			c, ok := decodeInvoiceCursor(*params.Cursor)
			if !ok {
				writeError(w, r, badCursor())
				return
			}
			after = &c
		}
		invs, next, err := s.bookings.OwnerInvoices(r.Context(), stringPtr(params.Status), after, pageLimit(params.Limit))
		if err != nil {
			writeError(w, r, err)
			return
		}
		for _, inv := range invs {
			page.Data = append(page.Data, invoiceBody(inv))
		}
		if next != nil {
			page.NextCursor = ptr(encodeInvoiceCursor(*next))
		}
		writeJSON(w, r, http.StatusOK, page)
	})(w, r)
}

// GetInvoice handles GET /invoices/{id}.
func (s *Server) GetInvoice(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermInvoicesRead, func(w http.ResponseWriter, r *http.Request) {
		inv, err := s.bookings.Invoice(r.Context(), id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, invoiceBody(inv))
	})(w, r)
}

func invoiceBody(inv booking.Invoice) Invoice {
	lines := make([]InvoiceLine, 0, len(inv.Lines))
	for _, l := range inv.Lines {
		lines = append(lines, InvoiceLine{Id: l.ID, Kind: InvoiceLineKind(l.Kind), Description: l.Description,
			Amount: l.Amount, HandoverPhotoId: l.HandoverPhotoID, WaiverReason: l.WaiverReason})
	}
	var cust *BookingCustomer
	if inv.Customer != nil {
		cust = &BookingCustomer{Id: inv.Customer.ID, Name: inv.Customer.Name,
			Phone: inv.Customer.Phone, IsBlacklisted: inv.Customer.Blacklisted}
	}
	return Invoice{Id: inv.ID, BookingId: inv.BookingID, Number: inv.Number, Status: InvoiceStatus(inv.Status),
		DueAt: inv.DueAt, PaidAt: inv.PaidAt, CreatedAt: inv.CreatedAt, Total: inv.Total, Lines: lines,
		Customer: cust}
}

func handoverInput(keys []string, meter *int64, checklist *map[string]any, notes *string) booking.HandoverInput {
	in := booking.HandoverInput{PhotoKeys: keys, MeterValue: meter, ConditionNotes: notes}
	if checklist != nil {
		in.Checklist = *checklist
	}
	return in
}

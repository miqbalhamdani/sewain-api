package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/miqbalhamdani/sewain-api/internal/booking"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
	"github.com/miqbalhamdani/sewain-api/internal/storage"
)

// The renter portal over HTTP.  (S1-053, S1-062, 04-api-spec.md §5)
//
// The owner came from Host (Lanes); the token names one booking of that owner
// and nothing else (BR-002). A token for another host, a tampered one and a
// booking that is gone all answer the same 404.

// portalBooking resolves the token, or writes the 404 and reports false.
func (s *Server) portalBooking(w http.ResponseWriter, r *http.Request, token PortalToken) (uuid.UUID, bool) {
	ownerID, _ := owner.FromContext(r.Context())
	id, ok := s.portal.Parse(ownerID, string(token))
	if !ok {
		writeError(w, r, apperrors.NotFound("No such booking."))
		return uuid.Nil, false
	}
	return id, true
}

// GetPortalBooking handles GET /portal/bookings/{token}.
func (s *Server) GetPortalBooking(w http.ResponseWriter, r *http.Request, token PortalToken) {
	id, ok := s.portalBooking(w, r, token)
	if !ok {
		return
	}
	v, err := s.bookings.Portal(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	b := v.Booking
	body := PortalBooking{
		Code: b.Code, Status: BookingStatus(b.Status), Overdue: b.Overdue,
		CustomerName: b.CustomerName, ResourceName: b.ResourceName,
		StartAt: b.StartAt, EndAt: b.EndAt, ActualReturnAt: b.ActualReturnAt,
		PricingUnit: PricingUnit(b.PricingUnit), DurationQty: int(b.DurationQty), Subtotal: b.Subtotal,
		Deposit:  PortalDeposit{State: PortalDepositState(v.DepositState), Amount: b.DepositAmount},
		Invoices: []PortalInvoice{}, Photos: []PortalPhoto{},
		Owner: PortalOwner{Name: v.Owner.Name, Whatsapp: v.Owner.WhatsApp, Address: v.Owner.Address},
	}
	if v.DepositState == "settled" {
		body.Deposit.Deducted, body.Deposit.Refunded = &b.DepositDeducted, &b.DepositRefunded
	}
	for _, inv := range v.Invoices {
		pi := PortalInvoice{Id: inv.ID, Number: inv.Number, Status: InvoiceStatus(inv.Status),
			DueAt: inv.DueAt, PaidAt: inv.PaidAt, Total: inv.Total, ProofPending: v.ProofPending[inv.ID]}
		for _, l := range inv.Lines {
			pi.Lines = append(pi.Lines, struct {
				Amount      int64  `json:"amount"`
				Description string `json:"description"`
				Kind        string `json:"kind"`
			}{Amount: l.Amount, Description: l.Description, Kind: l.Kind})
		}
		body.Invoices = append(body.Invoices, pi)
	}
	for _, p := range v.Photos {
		body.Photos = append(body.Photos, PortalPhoto{Direction: PortalPhotoDirection(p.Direction), Url: p.URL, TakenAt: p.TakenAt})
	}
	o := v.Owner
	if o.BankName != nil && o.BankAccountNumber != nil && o.BankAccountHolder != nil {
		body.Owner.Bank = &struct {
			AccountHolder string `json:"account_holder"`
			AccountNumber string `json:"account_number"`
			Name          string `json:"name"`
		}{AccountHolder: *o.BankAccountHolder, AccountNumber: *o.BankAccountNumber, Name: *o.BankName}
	}
	writeJSON(w, r, http.StatusOK, body)
}

// PresignPortalUpload handles POST /portal/bookings/{token}/uploads. The key
// sits under this booking, so it can only be handed in for it.
func (s *Server) PresignPortalUpload(w http.ResponseWriter, r *http.Request, token PortalToken) {
	id, ok := s.portalBooking(w, r, token)
	if !ok {
		return
	}
	if _, err := s.bookings.Get(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	var body PortalUploadRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, r, malformed(err))
		return
	}
	if !storage.ProofTypes[string(body.ContentType)] {
		writeError(w, r, apperrors.ValidationFailed("A proof is an image or a PDF.").
			WithFields(apperrors.Field{Name: "content_type"}))
		return
	}
	if body.Bytes < 1 || body.Bytes > storage.MaxUploadBytes {
		writeError(w, r, apperrors.ValidationFailed("A proof is at most 10 MB.").
			WithFields(apperrors.Field{Name: "bytes"}))
		return
	}
	ownerID, _ := owner.FromContext(r.Context())
	key := booking.PortalUploadKey(ownerID, id)
	url, err := s.objects.PresignPut(r.Context(), key, string(body.ContentType), body.Bytes)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, PresignedUpload{
		ObjectKey: key, UploadUrl: url, ExpiresIn: int(storage.UploadTTL.Seconds()),
		Headers: map[string]string{"Content-Type": string(body.ContentType)},
	})
}

// UploadPortalProof handles POST /portal/bookings/{token}/proofs: 202, and
// nothing is paid until a person approves it (BR-062).
func (s *Server) UploadPortalProof(w http.ResponseWriter, r *http.Request, token PortalToken) {
	id, ok := s.portalBooking(w, r, token)
	if !ok {
		return
	}
	var body PortalProofInput
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, r, malformed(err))
		return
	}
	p, err := s.bookings.UploadPortalProof(r.Context(), id, body.InvoiceId, body.ObjectKey)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusAccepted, PortalProofReceived{Id: p.ID, CreatedAt: p.CreatedAt})
}

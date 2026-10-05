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

// Deposit, payments and transfer proofs over HTTP.  (S1-042, S1-044, S1-046)

// PreviewDeposit handles GET /bookings/{id}/deposit.
func (s *Server) PreviewDeposit(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermBookingsRead, func(w http.ResponseWriter, r *http.Request) {
		p, err := s.bookings.DepositPreview(r.Context(), id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, DepositPreview{
			DepositAmount: p.DepositAmount, Collected: p.Collected, Waived: p.Waived, SettledAt: p.SettledAt,
			Deductions: p.Deductions, RefundAmount: p.RefundAmount, DeductedAmount: p.DeductedAmount,
			NewInvoiceAmount: p.NewInvoiceAmount,
		})
	})(w, r)
}

// SettleDeposit handles POST /bookings/{id}/deposit/settle.
func (s *Server) SettleDeposit(w http.ResponseWriter, r *http.Request, id uuid.UUID, _ SettleDepositParams) {
	requirePermission(auth.PermBookingsWrite, func(w http.ResponseWriter, r *http.Request) {
		var body SettleRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
			writeError(w, r, malformed(err))
			return
		}
		userID, _ := auth.UserFromContext(r.Context())
		b, err := s.bookings.Settle(r.Context(), userID, id, derefStr(body.Note))
		s.writeBooking(w, r, b, err)
	})(w, r)
}

// WaiveDeposit handles POST /bookings/{id}/deposit/waive. Owner only (M4).
func (s *Server) WaiveDeposit(w http.ResponseWriter, r *http.Request, id uuid.UUID, _ WaiveDepositParams) {
	requirePermission(auth.PermDepositsWaive, func(w http.ResponseWriter, r *http.Request) {
		var body WaiveRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
			writeError(w, r, malformed(err))
			return
		}
		userID, _ := auth.UserFromContext(r.Context())
		b, err := s.bookings.Waive(r.Context(), userID, id, body.Reason)
		s.writeBooking(w, r, b, err)
	})(w, r)
}

// CompleteBooking handles POST /bookings/{id}/complete.
func (s *Server) CompleteBooking(w http.ResponseWriter, r *http.Request, id uuid.UUID, _ CompleteBookingParams) {
	requirePermission(auth.PermBookingsWrite, func(w http.ResponseWriter, r *http.Request) {
		b, err := s.bookings.Complete(r.Context(), id)
		s.writeBooking(w, r, b, err)
	})(w, r)
}

// RecordPayment handles POST /invoices/{id}/payments.
func (s *Server) RecordPayment(w http.ResponseWriter, r *http.Request, id uuid.UUID, _ RecordPaymentParams) {
	requirePermission(auth.PermPaymentsWrite, func(w http.ResponseWriter, r *http.Request) {
		var body PaymentRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
			writeError(w, r, malformed(err))
			return
		}
		if body.Method != Cash && body.Method != ManualTransfer {
			writeError(w, r, apperrors.ValidationFailed("method is cash or manual_transfer.").
				WithFields(apperrors.Field{Name: "method"}))
			return
		}
		paidAt := time.Now()
		if body.PaidAt != nil {
			if body.PaidAt.After(paidAt.Add(time.Minute)) {
				writeError(w, r, apperrors.ValidationFailed("paid_at cannot be in the future.").
					WithFields(apperrors.Field{Name: "paid_at"}))
				return
			}
			paidAt = *body.PaidAt
		}
		userID, _ := auth.UserFromContext(r.Context())
		inv, err := s.bookings.RecordPayment(r.Context(), userID, id, string(body.Method), body.Amount, paidAt)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusCreated, invoiceBody(inv))
	})(w, r)
}

// ListProofs handles GET /invoices/{id}/proofs.
func (s *Server) ListProofs(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermInvoicesRead, func(w http.ResponseWriter, r *http.Request) {
		ps, err := s.bookings.Proofs(r.Context(), id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		out := make([]PaymentProof, 0, len(ps))
		for _, p := range ps {
			out = append(out, proofBody(p))
		}
		writeJSON(w, r, http.StatusOK, out)
	})(w, r)
}

// UploadProof handles POST /invoices/{id}/proofs -- 202, the reading follows.
func (s *Server) UploadProof(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermPaymentsWrite, func(w http.ResponseWriter, r *http.Request) {
		var body ProofInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
			writeError(w, r, malformed(err))
			return
		}
		userID, _ := auth.UserFromContext(r.Context())
		p, err := s.bookings.UploadProof(r.Context(), &userID, id, body.ObjectKey)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusAccepted, proofBody(p))
	})(w, r)
}

// ApproveProof handles POST /proofs/{id}/approve -- the person who marks paid.
func (s *Server) ApproveProof(w http.ResponseWriter, r *http.Request, id uuid.UUID, _ ApproveProofParams) {
	requirePermission(auth.PermPaymentsWrite, func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.UserFromContext(r.Context())
		inv, err := s.bookings.ApproveProof(r.Context(), userID, id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, invoiceBody(inv))
	})(w, r)
}

// RejectProof handles POST /proofs/{id}/reject.
func (s *Server) RejectProof(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermPaymentsWrite, func(w http.ResponseWriter, r *http.Request) {
		var body RejectRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
			writeError(w, r, malformed(err))
			return
		}
		userID, _ := auth.UserFromContext(r.Context())
		p, err := s.bookings.RejectProof(r.Context(), userID, id, body.Reason)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, proofBody(p))
	})(w, r)
}

func proofBody(p booking.Proof) PaymentProof {
	return PaymentProof{Id: p.ID, InvoiceId: p.InvoiceID, Url: p.URL, ContentType: p.ContentType,
		ReviewStatus: PaymentProofReviewStatus(p.ReviewStatus), MatchStatus: (*PaymentProofMatchStatus)(p.MatchStatus),
		AiAmount: p.AIAmount, AiPaidAt: p.AIPaidAt, ReviewedBy: p.ReviewedBy, ReviewedAt: p.ReviewedAt,
		RejectReason: p.RejectReason, CreatedAt: p.CreatedAt}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func encodeInvoiceCursor(c booking.InvoiceCursor) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID.String()))
}

func decodeInvoiceCursor(s string) (booking.InvoiceCursor, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return booking.InvoiceCursor{}, false
	}
	at, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return booking.InvoiceCursor{}, false
	}
	t, err1 := time.Parse(time.RFC3339Nano, at)
	u, err2 := uuid.Parse(id)
	if err1 != nil || err2 != nil {
		return booking.InvoiceCursor{}, false
	}
	return booking.InvoiceCursor{CreatedAt: t, ID: u}, true
}

package booking

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

// Payments and transfer proofs.  (S1-044, S1-046)
//
// Settlement is ALWAYS a person: recording cash or a checked transfer, or
// approving a proof. Nothing marks an invoice paid on its own (BR-062).

// Enqueuer is the job queue, as this package needs it (internal/jobs).
type Enqueuer interface {
	Enqueue(ctx context.Context, typ string, ownerID uuid.UUID, payload any) error
}

// Scanner reads a transfer proof. Phase 1 has no model: NoScanner returns
// ErrNotRead and the proof stays "not read, check by hand". A real reader plugs
// in here without touching the flow -- and its result is only ever a
// recommendation (BR-062).
type Scanner interface {
	Scan(ctx context.Context, url, contentType string) (Reading, error)
}

type Reading struct {
	MatchStatus string // match | mismatch | unreadable
	Amount      *int64
	PaidAt      *time.Time
}

var ErrNotRead = errors.New("no reader configured")

type NoScanner struct{}

func (NoScanner) Scan(context.Context, string, string) (Reading, error) { return Reading{}, ErrNotRead }

// JobScanProof is the worker job that reads one proof.
const JobScanProof = "proof.scan"

// WithJobs wires the queue and the reader. Without them, proofs are stored and
// approved exactly the same; only the reading never happens.
func (s *Service) WithJobs(q Enqueuer, scanner Scanner) *Service {
	s.jobs, s.scanner = q, scanner
	return s
}

func notFoundInvoice() error { return apperrors.NotFound("No such invoice in this business.") }

// RecordPayment is cash at the counter or a transfer the operator already
// checked (04-api-spec.md 3.8.1). Full payment only (BR-060).
func (s *Service) RecordPayment(ctx context.Context, actorID, invoiceID uuid.UUID, method string,
	amount int64, paidAt time.Time) (Invoice, error) {
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		inv, err := lockPayable(ctx, q, invoiceID)
		if err != nil {
			return err
		}
		if amount != inv.Total {
			return apperrors.ValidationFailed(fmt.Sprintf(
				"Payment is in full only: this invoice is %d.", inv.Total)).
				WithFields(apperrors.Field{Name: "amount"})
		}
		_, err = pay(ctx, q, actorID, invoiceID, method, amount, paidAt)
		return err
	})
	if err != nil {
		return Invoice{}, translatePayment(err)
	}
	return s.Invoice(ctx, invoiceID)
}

// lockPayable locks an invoice and refuses one that cannot take a payment.
func lockPayable(ctx context.Context, q *sqlcgen.Queries, id uuid.UUID) (sqlcgen.LockInvoiceRow, error) {
	inv, err := q.LockInvoice(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return inv, notFoundInvoice()
	}
	if err != nil {
		return inv, err
	}
	switch inv.Status {
	case "paid":
		return inv, apperrors.InvoiceAlreadyPaid()
	case "cancelled":
		return inv, apperrors.ValidationFailed("A cancelled invoice cannot be paid.").
			WithFields(apperrors.Field{Name: "status"})
	}
	if inv.Total <= 0 {
		return inv, apperrors.ValidationFailed("This invoice has nothing to pay.").
			WithFields(apperrors.Field{Name: "amount"})
	}
	return inv, nil
}

// pay writes the payment and marks the invoice paid, in the caller's tx.
func pay(ctx context.Context, q *sqlcgen.Queries, actorID, invoiceID uuid.UUID, method string,
	amount int64, paidAt time.Time) (uuid.UUID, error) {
	ownerID, _ := owner.FromContext(ctx)
	id := uuid.Must(uuid.NewV7())
	if err := q.InsertPayment(ctx, sqlcgen.InsertPaymentParams{
		ID: id, OwnerID: ownerID, InvoiceID: invoiceID, Method: method,
		Amount: amount, PaidAt: paidAt, ApprovedBy: actorID,
	}); err != nil {
		return id, err
	}
	return id, q.MarkInvoicePaid(ctx, sqlcgen.MarkInvoicePaidParams{ID: invoiceID, PaidAt: &paidAt})
}

func translatePayment(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "payments_one_success_per_invoice" {
		return apperrors.InvoiceAlreadyPaid()
	}
	return translate(err, "payment")
}

// Proof is one transfer proof with a short-lived read URL.
type Proof struct {
	ID           uuid.UUID
	InvoiceID    uuid.UUID
	URL          string
	ContentType  string
	ReviewStatus string
	MatchStatus  *string
	AIAmount     *int64
	AIPaidAt     *time.Time
	ReviewedBy   *string
	ReviewedAt   *time.Time
	RejectReason *string
	CreatedAt    time.Time
}

// UploadProof stores a proof and queues its reading. The reading is optional
// and late by design: a failed enqueue is logged, not returned -- the proof is
// already saved, and a person approves it either way (BR-062).
func (s *Service) UploadProof(ctx context.Context, actorID, invoiceID uuid.UUID, pendingKey string) (Proof, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return Proof{}, db.ErrNoOwnerContext
	}
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		_, err := lockPayable(ctx, sqlcgen.New(tx), invoiceID)
		return err
	})
	if err != nil {
		return Proof{}, translatePayment(err)
	}
	obj, final, err := s.objects.Promote(ctx, ownerID, pendingKey,
		"proofs/"+ownerID.String()+"/"+invoiceID.String(), "object_key", storage.ProofTypes)
	if err != nil {
		return Proof{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		return sqlcgen.New(tx).InsertProof(ctx, sqlcgen.InsertProofParams{
			ID: id, OwnerID: ownerID, InvoiceID: invoiceID, ObjectKey: final,
			ContentType: obj.ContentType, CreatedBy: &actorID,
		})
	}); err != nil {
		return Proof{}, fmt.Errorf("insert proof: %w", err)
	}
	if s.jobs != nil {
		if err := s.jobs.Enqueue(ctx, JobScanProof, ownerID, map[string]string{"proof_id": id.String()}); err != nil {
			slog.WarnContext(ctx, "proof saved but its reading was not queued", "proof_id", id, "error", err)
		}
	}
	return s.proof(ctx, id)
}

func (s *Service) Proofs(ctx context.Context, invoiceID uuid.UUID) ([]Proof, error) {
	var rows []sqlcgen.ListProofsRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if _, err := q.GetInvoice(ctx, invoiceID); err != nil {
			return err
		}
		var err error
		rows, err = q.ListProofs(ctx, invoiceID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFoundInvoice()
	}
	if err != nil {
		return nil, fmt.Errorf("list proofs: %w", err)
	}
	out := make([]Proof, 0, len(rows))
	for _, r := range rows {
		p, err := s.proofOf(ctx, sqlcgen.GetProofRow(r))
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// ApproveProof is the only way a transfer proof becomes paid: a person, in one
// transaction with the payment and the invoice (BR-062).
func (s *Service) ApproveProof(ctx context.Context, actorID, proofID uuid.UUID) (Invoice, error) {
	var invoiceID uuid.UUID
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		p, err := lockPendingProof(ctx, q, proofID)
		if err != nil {
			return err
		}
		invoiceID = p.InvoiceID
		inv, err := lockPayable(ctx, q, p.InvoiceID)
		if err != nil {
			return err
		}
		paymentID, err := pay(ctx, q, actorID, p.InvoiceID, "manual_transfer", inv.Total, time.Now())
		if err != nil {
			return err
		}
		return q.ApproveProof(ctx, sqlcgen.ApproveProofParams{ID: proofID, ReviewedBy: &actorID, PaymentID: &paymentID})
	})
	if err != nil {
		return Invoice{}, translatePayment(err)
	}
	return s.Invoice(ctx, invoiceID)
}

func (s *Service) RejectProof(ctx context.Context, actorID, proofID uuid.UUID, reason string) (Proof, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Proof{}, apperrors.ValidationFailed("A rejection needs a reason the renter can act on.").
			WithFields(apperrors.Field{Name: "reason"})
	}
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if _, err := lockPendingProof(ctx, q, proofID); err != nil {
			return err
		}
		return q.RejectProof(ctx, sqlcgen.RejectProofParams{ID: proofID, ReviewedBy: &actorID, RejectReason: &reason})
	})
	if err != nil {
		return Proof{}, translatePayment(err)
	}
	return s.proof(ctx, proofID)
}

func lockPendingProof(ctx context.Context, q *sqlcgen.Queries, id uuid.UUID) (sqlcgen.LockProofRow, error) {
	p, err := q.LockProof(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, apperrors.NotFound("No such proof in this business.")
	}
	if err != nil {
		return p, err
	}
	if p.ReviewStatus != "pending" {
		return p, apperrors.ValidationFailed("This proof was already " + p.ReviewStatus + ".").
			WithFields(apperrors.Field{Name: "review_status"})
	}
	return p, nil
}

// ScanProof is the proof.scan job handler. Idempotent by design (BR-091): a
// proof already read or already decided is left alone, and SetProofReading
// re-checks that under the update.
func (s *Service) ScanProof(ctx context.Context, proofID uuid.UUID) error {
	var p sqlcgen.GetProofForScanRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		p, err = sqlcgen.New(tx).GetProofForScan(ctx, proofID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // gone: nothing to read, nothing to retry
	}
	if err != nil {
		return err
	}
	if p.MatchStatus != nil || p.ReviewStatus != "pending" || s.scanner == nil {
		return nil
	}
	url, err := s.objects.PresignGet(ctx, p.ObjectKey, storage.ProofTTL)
	if err != nil {
		return err
	}
	r, err := s.scanner.Scan(ctx, url, p.ContentType)
	if errors.Is(err, ErrNotRead) {
		return nil // phase 1: stays "not read, check by hand"
	}
	if err != nil {
		return err
	}
	return s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		_, err := sqlcgen.New(tx).SetProofReading(ctx, sqlcgen.SetProofReadingParams{
			ID: proofID, MatchStatus: &r.MatchStatus, AiAmount: r.Amount, AiPaidAt: r.PaidAt})
		return err
	})
}

func (s *Service) proof(ctx context.Context, id uuid.UUID) (Proof, error) {
	var row sqlcgen.GetProofRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).GetProof(ctx, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Proof{}, apperrors.NotFound("No such proof in this business.")
	}
	if err != nil {
		return Proof{}, fmt.Errorf("get proof: %w", err)
	}
	return s.proofOf(ctx, row)
}

func (s *Service) proofOf(ctx context.Context, r sqlcgen.GetProofRow) (Proof, error) {
	url, err := s.objects.PresignGet(ctx, r.ObjectKey, storage.ProofTTL)
	if err != nil {
		return Proof{}, err
	}
	return Proof{ID: r.ID, InvoiceID: r.InvoiceID, URL: url, ContentType: r.ContentType,
		ReviewStatus: r.ReviewStatus, MatchStatus: r.MatchStatus, AIAmount: r.AiAmount, AIPaidAt: r.AiPaidAt,
		ReviewedBy: r.ReviewedByName, ReviewedAt: r.ReviewedAt, RejectReason: r.RejectReason,
		CreatedAt: r.CreatedAt}, nil
}

// InvoiceCursor is the keyset position of the owner's invoice list.
type InvoiceCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// OwnerInvoices is the full list across bookings (S1-047), newest first.
func (s *Service) OwnerInvoices(ctx context.Context, status *string, after *InvoiceCursor, limit int) ([]Invoice, *InvoiceCursor, error) {
	params := sqlcgen.ListOwnerInvoicesParams{Status: status, Lim: int32(limit + 1)} //nolint:gosec // bounded by the schema
	if after != nil {
		params.CursorAt, params.CursorID = &after.CreatedAt, &after.ID
	}
	var out []Invoice
	var more bool
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		rows, err := q.ListOwnerInvoices(ctx, params)
		if err != nil {
			return err
		}
		// One extra row is read only to know whether another page exists.
		if more = len(rows) > limit; more {
			rows = rows[:limit]
		}
		out = make([]Invoice, 0, len(rows))
		for _, r := range rows {
			out = append(out, invoiceOf(sqlcgen.GetInvoiceRow(r)))
		}
		return attachLines(ctx, q, out)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list invoices: %w", err)
	}
	var next *InvoiceCursor
	if more {
		last := out[len(out)-1]
		next = &InvoiceCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return out, next, nil
}

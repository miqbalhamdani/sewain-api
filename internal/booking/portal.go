package booking

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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
)

// Renter portal.  (S1-053, S1-062, BR-002, 04-api-spec.md §5)
//
// The token is the booking id plus an HMAC of owner and booking, cut to 16
// bytes -- 43 base64url characters, stored nowhere. The same link every time,
// and it only works on its owner's host: the HMAC covers the owner the Host
// lookup resolved, so a token carried to another rental's host fails like a
// forged one. Rotating PORTAL_SECRET revokes every link at once.

const portalMACBytes = 16

// PortalLinks mints and checks portal tokens.
type PortalLinks struct {
	secret       []byte
	originFormat string // fmt pattern for <slug> → origin
}

func NewPortalLinks(secret, originFormat string) PortalLinks {
	return PortalLinks{secret: []byte(secret), originFormat: originFormat}
}

func (p PortalLinks) mac(ownerID, bookingID uuid.UUID) []byte {
	m := hmac.New(sha256.New, p.secret)
	m.Write(ownerID[:])
	m.Write(bookingID[:])
	return m.Sum(nil)[:portalMACBytes]
}

// Token is the portal token for one booking of one rental.
func (p PortalLinks) Token(ownerID, bookingID uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString(append(bookingID[:], p.mac(ownerID, bookingID)...))
}

// Parse returns the booking a token names, if it was minted for this owner.
func (p PortalLinks) Parse(ownerID uuid.UUID, token string) (uuid.UUID, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 16+portalMACBytes {
		return uuid.Nil, false
	}
	id, _ := uuid.FromBytes(raw[:16])
	return id, hmac.Equal(raw[16:], p.mac(ownerID, id))
}

// URL is the link staff send the renter, or nil while the rental has no slug
// -- the portal lives on the owner's host, and there is none yet.
func (p PortalLinks) URL(ownerID, bookingID uuid.UUID, slug *string) *string {
	if slug == nil || *slug == "" {
		return nil
	}
	u := fmt.Sprintf(p.originFormat, *slug) + "/booking/" + p.Token(ownerID, bookingID)
	return &u
}

// PortalView is one booking as its renter sees it: no unit code, no staff
// names, no internal notes (BR-002).
type PortalView struct {
	Booking      Booking
	Invoices     []Invoice
	ProofPending map[uuid.UUID]bool
	DepositState string // none | unpaid | held | waived | settled
	Photos       []PortalPhoto
	Owner        OwnerProfile
}

type PortalPhoto struct {
	Direction string
	URL       string
	TakenAt   time.Time
}

// OwnerProfile is what renters see of the business (BR-096, S1-062).
type OwnerProfile struct {
	Name                                           string
	Slug, WhatsApp, Address, OperatingHours        *string
	BankName, BankAccountNumber, BankAccountHolder *string
}

// Profile reads the business card of the rental in the context.
func (s *Service) Profile(ctx context.Context) (OwnerProfile, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return OwnerProfile{}, db.ErrNoOwnerContext
	}
	var r sqlcgen.GetOwnerProfileRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		r, err = sqlcgen.New(tx).GetOwnerProfile(ctx, ownerID)
		return err
	})
	if err != nil {
		return OwnerProfile{}, fmt.Errorf("owner profile: %w", err)
	}
	return OwnerProfile{Name: r.Name, Slug: r.Slug, WhatsApp: r.Whatsapp, Address: r.Address,
		OperatingHours: r.OperatingHours, BankName: r.BankName,
		BankAccountNumber: r.BankAccountNumber, BankAccountHolder: r.BankAccountHolder}, nil
}

// Portal assembles the renter's page from the same reads the backoffice uses;
// the trimming happens where it becomes the contract type.
func (s *Service) Portal(ctx context.Context, bookingID uuid.UUID) (PortalView, error) {
	b, err := s.Get(ctx, bookingID)
	if err != nil {
		return PortalView{}, err
	}
	v := PortalView{Booking: b, ProofPending: map[uuid.UUID]bool{}}
	if v.Invoices, err = s.Invoices(ctx, bookingID); err != nil {
		return PortalView{}, err
	}
	err = s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		ids, err := sqlcgen.New(tx).PendingProofInvoices(ctx, &bookingID)
		for _, id := range ids {
			v.ProofPending[id] = true
		}
		return err
	})
	if err != nil {
		return PortalView{}, fmt.Errorf("pending proofs: %w", err)
	}
	hs, err := s.Handovers(ctx, bookingID)
	if err != nil {
		return PortalView{}, err
	}
	for _, h := range hs {
		for _, ph := range h.Photos {
			v.Photos = append(v.Photos, PortalPhoto{Direction: h.Direction, URL: ph.URL, TakenAt: ph.CapturedAt})
		}
	}
	v.DepositState = depositState(b, v.Invoices)
	if v.Owner, err = s.Profile(ctx); err != nil {
		return PortalView{}, err
	}
	return v, nil
}

// depositState reads the deposit without LockBookingDeposit's FOR UPDATE: a
// renter refreshing a page must never queue behind a settlement.
func depositState(b Booking, invs []Invoice) string {
	switch {
	case b.DepositAmount == nil:
		return "none"
	case b.DepositWaivedAt != nil:
		return "waived"
	case b.DepositSettledAt != nil:
		return "settled"
	}
	for _, inv := range invs {
		if inv.Status != "paid" {
			continue
		}
		for _, l := range inv.Lines {
			if l.Kind == "deposit" {
				return "held"
			}
		}
	}
	return "unpaid"
}

// PortalUploadKey is where a renter's proof lands before it is handed in:
// under this booking, so /proofs can refuse a key minted for another one.
func PortalUploadKey(ownerID, bookingID uuid.UUID) string {
	return portalPrefix(ownerID, bookingID) + uuid.Must(uuid.NewV7()).String()
}

func portalPrefix(ownerID, bookingID uuid.UUID) string {
	return "pending/" + ownerID.String() + "/" + bookingID.String() + "/"
}

// UploadPortalProof hands in a renter's proof for one invoice of the token's
// booking (BR-062). Another booking's invoice, or a key from another booking's
// presign, is a 404 -- the token reaches this booking and nothing else.
func (s *Service) UploadPortalProof(ctx context.Context, bookingID, invoiceID uuid.UUID, key string) (Proof, error) {
	ownerID, ok := owner.FromContext(ctx)
	if !ok {
		return Proof{}, db.ErrNoOwnerContext
	}
	if !strings.HasPrefix(key, portalPrefix(ownerID, bookingID)) {
		// 404, not upload-not-found: from the renter's side a key of another
		// booking is as unreachable as another booking (04-api-spec.md §5).
		return Proof{}, apperrors.NotFound("No such upload for this booking.")
	}
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		inv, err := sqlcgen.New(tx).GetInvoice(ctx, invoiceID)
		if err != nil {
			return err
		}
		if inv.BookingID == nil || *inv.BookingID != bookingID {
			return pgx.ErrNoRows
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Proof{}, notFoundInvoice()
	}
	if err != nil {
		return Proof{}, fmt.Errorf("portal proof: %w", err)
	}
	return s.UploadProof(ctx, nil, invoiceID, key)
}

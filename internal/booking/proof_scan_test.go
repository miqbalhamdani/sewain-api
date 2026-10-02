package booking

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/miqbalhamdani/sewain-api/internal/storage"
)

type fakeScanner struct{ calls atomic.Int32 }

func (f *fakeScanner) Scan(context.Context, string, string) (Reading, error) {
	f.calls.Add(1)
	amount := int64(700000)
	at := time.Now()
	return Reading{MatchStatus: "match", Amount: &amount, PaidAt: &at}, nil
}

// BR-091 + BR-062: proof.scan is idempotent -- a second delivery of the same
// job does not read twice -- and a reading never touches a proof a person has
// already decided. This is the seam a real reader plugs into; phase 1 runs
// NoScanner, which leaves every proof unread.
func TestScanProof(t *testing.T) {
	f := newFixture(t, 1)
	objects, err := storage.FromConfig()
	if err != nil {
		t.Fatal(err)
	}
	scanner := &fakeScanner{}
	s := New(f.store, objects).WithJobs(nil, scanner)

	invoice, pending, decided := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	booking := uuid.Must(uuid.NewV7())
	f.exec(t, `INSERT INTO bookings (id, owner_id, code, customer_id, resource_id, resource_unit_id, start_at, end_at,
	             status, unit_price, pricing_unit, duration_qty, subtotal)
	           VALUES ($1, $2, 'SC-1', $3, $4, $5, now(), now() + interval '1 day', 'reserved', 1, 'day', 1, 1)`,
		booking, f.ownerID, f.customer, f.resource, f.units[0])
	f.exec(t, `INSERT INTO invoices (id, owner_id, booking_id, number, due_at) VALUES ($1, $2, $3, 'SC-1/1', now())`,
		invoice, f.ownerID, booking)
	f.exec(t, `INSERT INTO payment_proofs (id, owner_id, invoice_id, object_key, content_type) VALUES ($1, $2, $3, 'k', 'image/jpeg')`,
		pending, f.ownerID, invoice)
	f.exec(t, `INSERT INTO payment_proofs (id, owner_id, invoice_id, object_key, content_type, review_status,
	             reviewed_by, reviewed_at, reject_reason)
	           VALUES ($1, $2, $3, 'k', 'image/jpeg', 'rejected', $4, now(), 'buram')`,
		decided, f.ownerID, invoice, f.userID)

	for range 2 { // the same job delivered twice
		if err := s.ScanProof(f.ctx, pending); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ScanProof(f.ctx, decided); err != nil {
		t.Fatal(err)
	}
	if n := scanner.calls.Load(); n != 1 {
		t.Errorf("scanner called %d times, want 1: once for the unread proof, never again, never for a decided one", n)
	}
	p, err := s.proof(f.ctx, pending)
	if err != nil || p.MatchStatus == nil || *p.MatchStatus != "match" || p.ReviewStatus != "pending" {
		t.Fatalf("read proof = %+v %v -- a reading is a recommendation, never a decision", p, err)
	}
	if err := New(f.store, objects).WithJobs(nil, NoScanner{}).ScanProof(f.ctx, uuid.Must(uuid.NewV7())); err != nil {
		t.Errorf("a proof that no longer exists must be a no-op, got %v", err)
	}
}

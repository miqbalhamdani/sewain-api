package booking

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// S1-052 against real PostgreSQL: every transition, the handover guard, both
// positions of the pay-first switch, and a second run that changes nothing.
func TestExpire(t *testing.T) {
	f := newFixture(t, 6)
	s := New(f.store, nil)
	now := time.Now()

	book := func(code, status string, unit int, start time.Time, expires *time.Time) uuid.UUID {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		f.exec(t, `INSERT INTO bookings (id, owner_id, code, customer_id, resource_id, resource_unit_id,
		             start_at, end_at, status, unit_price, pricing_unit, duration_qty, subtotal, expires_at)
		           VALUES ($1, $2, $3, $4, $5, $6, $7, $7::timestamptz + interval '1 day', $8, 1, 'day', 1, 1, $9)`,
			id, f.ownerID, code, f.customer, f.resource, f.units[unit], start, status, expires)
		return id
	}
	invoice := func(booking uuid.UUID, due time.Time, status string) uuid.UUID {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		f.exec(t, `INSERT INTO invoices (id, owner_id, booking_id, number, due_at, status) VALUES ($1, $2, $3, $4, $5, $6)`,
			id, f.ownerID, booking, "INV-"+id.String(), due, status)
		f.exec(t, `INSERT INTO invoice_lines (id, owner_id, invoice_id, kind, description, amount)
		           VALUES ($1, $2, $3, 'rent', 'Sewa', 1)`, uuid.Must(uuid.NewV7()), f.ownerID, id)
		return id
	}
	statusOf := func(table string, id uuid.UUID) string {
		t.Helper()
		var st string
		if err := f.store.InOwnerTx(f.ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(f.ctx, `SELECT status FROM `+table+` WHERE id = $1`, id).Scan(&st)
		}); err != nil {
			t.Fatal(err)
		}
		return st
	}
	past, future := now.Add(-2*time.Hour), now.Add(48*time.Hour)

	// Drafts: one expired, one not yet.
	oldDraft := book("D-OLD", "draft", 0, future, &past)
	newDraft := book("D-NEW", "draft", 1, future.Add(72*time.Hour), &future)

	// Switch OFF first: unpaid past due only becomes an overdue invoice.
	unpaidOff := book("R-OFF", "reserved", 2, future, nil)
	invOff := invoice(unpaidOff, past, "unpaid")

	// No-show: reserved, started 4h ago (tolerance 3h), invoice paid.
	late := book("R-LATE", "reserved", 3, now.Add(-4*time.Hour), nil)
	invoice(late, now.Add(-5*time.Hour), "paid")

	// Same as late but a handover exists: untouchable.
	guarded := book("R-GUARD", "reserved", 4, now.Add(-4*time.Hour), nil)
	f.exec(t, `INSERT INTO handovers (id, owner_id, booking_id, direction, performed_by) VALUES ($1, $2, $3, 'pickup', $4)`,
		uuid.Must(uuid.NewV7()), f.ownerID, guarded, f.userID)

	got, err := s.Expire(f.ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Drafts != 1 || got.PaymentExpired != 0 || got.NoShows != 1 || got.OverdueInvoices != 1 {
		t.Errorf("switch off run = %+v, want 1 draft, 0 payment-expired, 1 no-show, 1 overdue invoice", got)
	}
	for id, want := range map[uuid.UUID]string{oldDraft: "cancelled", newDraft: "draft", unpaidOff: "reserved",
		late: "no_show", guarded: "reserved"} {
		if st := statusOf("bookings", id); st != want {
			t.Errorf("booking %s = %s, want %s", id, st, want)
		}
	}
	if st := statusOf("invoices", invOff); st != "overdue" {
		t.Errorf("unpaid invoice with switch off = %s, want overdue (BR-038)", st)
	}

	// Switch ON: the same unpaid booking is now cancelled for non-payment, and
	// its overdue invoice is cancelled with it -- the manual-cancel rule.
	f.exec(t, `UPDATE owners SET require_payment_before_pickup = true WHERE id = $1`, f.ownerID)
	got, err = s.Expire(f.ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.PaymentExpired != 1 {
		t.Errorf("switch on run = %+v, want 1 payment-expired", got)
	}
	if st := statusOf("bookings", unpaidOff); st != "cancelled" {
		t.Errorf("unpaid booking with switch on = %s, want cancelled", st)
	}
	if st := statusOf("invoices", invOff); st != "cancelled" {
		t.Errorf("its invoice = %s, want cancelled (BR-057 revision)", st)
	}

	// A third run finds nothing: the job is at-least-once (BR-091).
	if got, err = s.Expire(f.ctx, now); err != nil || got != (Expired{}) {
		t.Errorf("re-run = %+v, %v; want nothing changed", got, err)
	}
}

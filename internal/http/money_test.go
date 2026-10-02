package httpapi_test

import (
	"bytes"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// The acceptance for M4 -- S1-042, S1-044, S1-046 -- through the real router.
// The fixture's booking ISO-0001 has invoice ISO-0001/1: rent 700000.

func (c bookingClient) invoices() []map[string]any {
	c.t.Helper()
	m := expect(c.t, c.do(http.MethodGet, "/api/v1/invoices?booking_id="+c.s.bookingID, ""), http.StatusOK, "")
	out := []map[string]any{}
	for _, v := range m["data"].([]any) {
		out = append(out, v.(map[string]any))
	}
	return out
}

// BR-060: full payment only, one success per invoice -- also when two
// requests race, which the unique index decides.
func TestRecordPayment(t *testing.T) {
	c := newBookingClient(t)
	pay := "/api/v1/invoices/" + c.s.invoiceID + "/payments"

	expect(t, c.action(pay, `{"method":"cash","amount":100}`), http.StatusUnprocessableEntity, "validation-failed")

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = c.action(pay, `{"method":"cash","amount":700000}`).Code
		}()
	}
	wg.Wait()
	if one := codes[0] == 201 && codes[1] == 409 || codes[0] == 409 && codes[1] == 201; !one {
		t.Fatalf("two concurrent payments = %v, want exactly one 201 and one 409", codes)
	}
	if inv := c.invoices()[0]; inv["status"] != "paid" || inv["paid_at"] == nil {
		t.Errorf("invoice after payment: %v", inv)
	}
	expect(t, c.action(pay, `{"method":"manual_transfer","amount":700000}`), http.StatusConflict, "invoice-already-paid")
}

// setDeposit gives the fixture booking a paid-or-unpaid deposit line, and puts
// it in the returned state with an unpaid return invoice carrying a late fee.
func (c bookingClient) returnedWithDeposit(deposit, lateFee int64, depositPaid bool) {
	c.t.Helper()
	c.exec(`UPDATE bookings SET deposit_amount = $2, status = 'returned', actual_return_at = now() WHERE id = $1`,
		c.s.bookingID, deposit)
	c.exec(`INSERT INTO invoice_lines (id, owner_id, invoice_id, kind, description, amount)
	        VALUES ($1, $2, $3, 'deposit', 'Deposit', $4)`, uuid.Must(uuid.NewV7()), c.ownerID, c.s.invoiceID, deposit)
	if depositPaid {
		c.exec(`UPDATE invoices SET status = 'paid', paid_at = now() WHERE id = $1`, c.s.invoiceID)
	}
	ret := uuid.Must(uuid.NewV7())
	c.exec(`INSERT INTO invoices (id, owner_id, booking_id, customer_id, number, due_at)
	        VALUES ($1, $2, $3, $4, 'ISO-0001/2', now() + interval '1 day')`, ret, c.ownerID, c.s.bookingID, c.s.customerID)
	c.exec(`INSERT INTO invoice_lines (id, owner_id, invoice_id, kind, description, amount)
	        VALUES ($1, $2, $3, 'late_fee', 'Telat 2 hari', $4)`, uuid.Must(uuid.NewV7()), c.ownerID, ret, lateFee)
}

// BR-048 + BR-049: the deposit absorbs the return charges -- the return
// invoice is cancelled, not paid twice -- and the rest goes back.
func TestSettleRefund(t *testing.T) {
	c := newBookingClient(t)
	c.returnedWithDeposit(500000, 200000, false)
	base := "/api/v1/bookings/" + c.s.bookingID

	expect(t, c.action(base+"/complete", ``), http.StatusConflict, "deposit-not-settled")
	expect(t, c.action(base+"/deposit/settle", `{"note":"telat"}`), http.StatusConflict, "deposit-not-collected")
	c.exec(`UPDATE invoices SET status = 'paid', paid_at = now() WHERE id = $1`, c.s.invoiceID)

	p := expect(t, c.do(http.MethodGet, base+"/deposit", ""), http.StatusOK, "")
	if p["deductions"] != float64(200000) || p["refund_amount"] != float64(300000) || p["new_invoice_amount"] != float64(0) {
		t.Fatalf("preview = %v", p)
	}
	expect(t, c.action(base+"/deposit/settle", `{}`), http.StatusUnprocessableEntity, "validation-failed")
	b := expect(t, c.action(base+"/deposit/settle", `{"note":"Potong denda telat"}`), http.StatusOK, "")
	if b["deposit_deducted"] != float64(200000) || b["deposit_refunded"] != float64(300000) || b["deposit_settled_at"] == nil {
		t.Fatalf("after settle: %v", b)
	}
	invs := c.invoices()
	if len(invs) != 2 || invs[1]["status"] != "cancelled" {
		t.Fatalf("the return invoice must be absorbed (cancelled), got %v", invs)
	}
	expect(t, c.action(base+"/deposit/settle", `{"note":"lagi"}`), http.StatusUnprocessableEntity, "validation-failed")
	if b := expect(t, c.action(base+"/complete", ``), http.StatusOK, ""); b["status"] != "completed" {
		t.Errorf("complete = %v", b["status"])
	}
}

// BR-048: a shortfall is a new invoice for the difference, never a negative
// deposit.
func TestSettleShortfall(t *testing.T) {
	c := newBookingClient(t)
	c.returnedWithDeposit(500000, 700000, true)
	b := expect(t, c.action("/api/v1/bookings/"+c.s.bookingID+"/deposit/settle", `{"note":"Telat 7 hari"}`), http.StatusOK, "")
	if b["deposit_deducted"] != float64(500000) || b["deposit_refunded"] != float64(0) {
		t.Fatalf("after settle: %v", b)
	}
	invs := c.invoices()
	if len(invs) != 3 || invs[2]["number"] != "ISO-0001/3" || invs[2]["total"] != float64(200000) {
		t.Fatalf("invoices = %v, want a third for the 200000 shortfall", invs)
	}
}

// BR-051 as decided in M4: the owner waives an unpaid deposit by removing the
// line; once paid it is closed; an operator cannot; a waived deposit never
// blocks complete.
func TestWaiveDeposit(t *testing.T) {
	c := newBookingClient(t)
	c.returnedWithDeposit(500000, 1, false)
	waive := "/api/v1/bookings/" + c.s.bookingID + "/deposit/waive"

	expect(t, c.action(waive, `{"reason":"  "}`), http.StatusUnprocessableEntity, "waiver-reason-required")
	b := expect(t, c.action(waive, `{"reason":"Pelanggan tetap"}`), http.StatusOK, "")
	if b["deposit_waived_at"] == nil {
		t.Fatalf("not waived: %v", b)
	}
	if inv := c.invoices()[0]; inv["total"] != float64(700000) {
		t.Errorf("first invoice total = %v, want 700000 with the deposit line removed", inv["total"])
	}
	if n := c.count(`SELECT count(*) FROM audit_logs WHERE entity_id = $1 AND action = 'booking.deposit.waived'`,
		c.s.bookingID); n != 1 {
		t.Errorf("audit rows = %d", n)
	}
	expect(t, c.action(waive, `{"reason":"lagi"}`), http.StatusConflict, "deposit-not-applicable")
	expect(t, c.action("/api/v1/bookings/"+c.s.bookingID+"/complete", ``), http.StatusOK, "")

	// Paid: closed for good.
	d := newBookingClient(t)
	d.returnedWithDeposit(500000, 1, true)
	expect(t, d.action("/api/v1/bookings/"+d.s.bookingID+"/deposit/waive", `{"reason":"x"}`),
		http.StatusConflict, "deposit-already-paid")

	// Operator: the permission is not theirs.
	ctx := t.Context()
	store := openAppStore(ctx, t)
	op := seedSignedInUser(ctx, t, store, uuid.Must(uuid.NewV7()))
	w := bookingClient{catalogClient: catalogClient{t: t, srv: c.srv, token: op.accessToken}}.
		action("/api/v1/bookings/"+uuid.Must(uuid.NewV7()).String()+"/deposit/waive", `{"reason":"x"}`)
	expect(t, w, http.StatusForbidden, "permission-denied")
}

// BR-062 + BR-093: a proof (PDF allowed for this kind only) is stored and
// queued, 202; it reads "not read" in phase 1; only a person's approval makes
// the invoice paid; a rejection needs a reason.
func TestProofFlow(t *testing.T) {
	c := newBookingClient(t)
	expect(t, c.do(http.MethodPost, "/api/v1/uploads/presign",
		`{"kind":"handover_photo","content_type":"application/pdf","bytes":10}`), http.StatusUnprocessableEntity, "validation-failed")

	upload := func() string {
		body := []byte("%PDF-1.4 bukti transfer")
		m := expect(t, c.do(http.MethodPost, "/api/v1/uploads/presign",
			`{"kind":"payment_proof","content_type":"application/pdf","bytes":`+strconv.Itoa(len(body))+`}`),
			http.StatusCreated, "")
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, m["upload_url"].(string), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/pdf")
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("PUT proof: %v %v", err, res)
		}
		_ = res.Body.Close()
		return m["object_key"].(string)
	}
	proofs := "/api/v1/invoices/" + c.s.invoiceID + "/proofs"
	first := expect(t, c.do(http.MethodPost, proofs, `{"object_key":"`+upload()+`"}`), http.StatusAccepted, "")
	if first["review_status"] != "pending" || first["match_status"] != nil || first["content_type"] != "application/pdf" {
		t.Fatalf("new proof = %v", first)
	}
	second := expect(t, c.do(http.MethodPost, proofs, `{"object_key":"`+upload()+`"}`), http.StatusAccepted, "")
	if inv := c.invoices()[0]; inv["status"] != "unpaid" {
		t.Fatalf("a proof alone made the invoice %v -- only approval may (BR-062)", inv["status"])
	}

	expect(t, c.do(http.MethodPost, "/api/v1/proofs/"+second["id"].(string)+"/reject", `{"reason":""}`),
		http.StatusUnprocessableEntity, "validation-failed")
	rej := expect(t, c.do(http.MethodPost, "/api/v1/proofs/"+second["id"].(string)+"/reject",
		`{"reason":"Nominal tidak terbaca"}`), http.StatusOK, "")
	if rej["review_status"] != "rejected" || rej["reviewed_by"] == nil {
		t.Errorf("rejected proof = %v", rej)
	}

	inv := expect(t, c.action("/api/v1/proofs/"+first["id"].(string)+"/approve", ``), http.StatusOK, "")
	if inv["status"] != "paid" {
		t.Fatalf("approve: invoice %v", inv["status"])
	}
	expect(t, c.action("/api/v1/proofs/"+first["id"].(string)+"/approve", ``), http.StatusUnprocessableEntity, "validation-failed")
	expect(t, c.do(http.MethodPost, proofs, `{"object_key":"`+upload()+`"}`), http.StatusConflict, "invoice-already-paid")
}

package httpapi_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/owner"
)

// The acceptance for M3 -- S1-021, S1-033 .. S1-036, S1-041 -- through the real
// router, real PostgreSQL and the real local MinIO. Photos are uploaded the
// way a browser does it: presign, then PUT straight to the store.

// upload presigns and PUTs a tiny JPEG, returning its pending key.
func (c bookingClient) upload(kind string) string {
	c.t.Helper()
	body := bytes.Repeat([]byte{0xff, 0xd8}, 256)
	m := expect(c.t, c.do(http.MethodPost, "/api/v1/uploads/presign",
		`{"kind":"`+kind+`","content_type":"image/jpeg","bytes":`+strconv.Itoa(len(body))+`}`),
		http.StatusCreated, "")
	req, _ := http.NewRequestWithContext(c.t.Context(), http.MethodPut, m["upload_url"].(string), bytes.NewReader(body))
	req.Header.Set("Content-Type", "image/jpeg")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		c.t.Fatalf("PUT to presigned URL = %d", res.StatusCode)
	}
	return m["object_key"].(string)
}

// action POSTs with an Idempotency-Key, as pickup and return require.
func (c bookingClient) action(path, body string) *httptest.ResponseRecorder {
	c.t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("Idempotency-Key", uuid.Must(uuid.NewV7()).String())
	w := httptest.NewRecorder()
	c.srv.ServeHTTP(w, r)
	return w
}

func (c bookingClient) count(sql string, args ...any) int {
	c.t.Helper()
	var n int
	ctx := context.WithoutCancel(c.t.Context())
	if err := c.store.InOwnerTx(owner.NewContext(ctx, c.ownerID), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	}); err != nil {
		c.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// BR-036 + BR-093 + BR-035: a pickup needs a photo the store has actually seen
// and, for a vehicle, the odometer; it writes one handover and moves the unit.
func TestPickup(t *testing.T) {
	c := newBookingClient(t)
	pickup := "/api/v1/bookings/" + c.s.bookingID + "/pickup"

	expect(t, c.action(pickup, `{"photo_keys":[],"meter_value":1}`), http.StatusUnprocessableEntity, "handover-photo-required")
	key := c.upload("handover_photo")
	expect(t, c.action(pickup, `{"photo_keys":["`+key+`"]}`), http.StatusUnprocessableEntity, "meter-value-required")
	expect(t, c.action(pickup, `{"photo_keys":["pending/`+c.ownerID.String()+`/made-up"],"meter_value":1}`),
		http.StatusUnprocessableEntity, "upload-not-found")
	foreign := "pending/" + uuid.Must(uuid.NewV7()).String() + "/x"
	expect(t, c.action(pickup, `{"photo_keys":["`+foreign+`"],"meter_value":1}`),
		http.StatusUnprocessableEntity, "upload-not-found")
	if n := c.count(`SELECT count(*) FROM handovers WHERE booking_id = $1`, c.s.bookingID); n != 0 {
		t.Fatalf("refused pickups left %d handover rows", n)
	}

	b := expect(t, c.action(pickup, `{"photo_keys":["`+key+`"],"meter_value":45120,"checklist":{"bensin":"penuh"}}`),
		http.StatusOK, "")
	if b["status"] != "picked_up" {
		t.Fatalf("status = %v, want picked_up", b["status"])
	}
	if n := c.count(`SELECT meter_value FROM resource_units WHERE id = $1`, c.s.unitID); n != 45120 {
		t.Errorf("unit meter = %d, want 45120", n)
	}
	expect(t, c.action(pickup, `{"photo_keys":["`+key+`"],"meter_value":1}`), http.StatusUnprocessableEntity, "validation-failed")

	w := c.do(http.MethodGet, "/api/v1/bookings/"+c.s.bookingID+"/handovers", "")
	if w.Code != http.StatusOK {
		t.Fatalf("handovers = %d %s", w.Code, w.Body)
	}
	var hs []map[string]any
	_ = jsonUnmarshal(w.Body.Bytes(), &hs)
	if len(hs) != 1 || hs[0]["direction"] != "pickup" || len(hs[0]["photos"].([]any)) != 1 {
		t.Fatalf("handovers = %v", hs)
	}
	url := hs[0]["photos"].([]any)[0].(map[string]any)["url"].(string)
	if !strings.Contains(url, "/handovers/"+c.ownerID.String()+"/") {
		t.Errorf("photo was not copied to its final prefix: %s", url)
	}
	res, err := http.Get(url)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("signed photo URL: %v %v", err, res)
	}
	_ = res.Body.Close()
}

// BR-038: with pay-first on, an unpaid rent invoice blocks the pickup; paid,
// it goes through.
func TestPayBeforePickup(t *testing.T) {
	c := newBookingClient(t)
	c.exec(`UPDATE owners SET require_payment_before_pickup = true WHERE id = $1`, c.ownerID)
	pickup := "/api/v1/bookings/" + c.s.bookingID + "/pickup"
	body := `{"photo_keys":["` + c.upload("handover_photo") + `"],"meter_value":1}`

	expect(t, c.action(pickup, body), http.StatusConflict, "payment-required-before-pickup")
	c.exec(`UPDATE invoices SET status = 'paid', paid_at = now() WHERE id = $1`, c.s.invoiceID)
	expect(t, c.action(pickup, body), http.StatusOK, "")
}

// BR-042: the unit is still out with an earlier booking past its end. The
// first try names it; the explicit confirmation goes through.
func TestPhysicalConflict(t *testing.T) {
	c := newBookingClient(t)
	c.exec(`UPDATE bookings SET status = 'picked_up' WHERE id = $1`, c.s.bookingID) // 3-5 Sep 2026: long overdue
	next := expect(t, c.book("2027-03-01T09:00:00+07:00", "2027-03-02T09:00:00+07:00"), http.StatusCreated, "")
	pickup := "/api/v1/bookings/" + next["id"].(string) + "/pickup"
	key := c.upload("handover_photo")

	m := expect(t, c.action(pickup, `{"photo_keys":["`+key+`"],"meter_value":1}`), http.StatusConflict, "physical-conflict-unconfirmed")
	if cs := m["conflicts"].([]any); len(cs) != 1 || cs[0].(map[string]any)["code"] != "ISO-0001" {
		t.Fatalf("conflicts = %v", m["conflicts"])
	}
	expect(t, c.action(pickup, `{"photo_keys":["`+key+`"],"meter_value":1,"confirm_physical_conflict":true}`), http.StatusOK, "")
}

// BR-046 + BR-051 + BR-047 + BR-040: the fee is previewed, only what is
// confirmed is issued, every waiver has a reason, and a damage names one of
// the return's own photos.
func TestReturn(t *testing.T) {
	c := newBookingClient(t)
	c.exec(`UPDATE bookings SET status = 'picked_up', late_fee_per_unit = 100000,
	          start_at = now() - interval '5 days', end_at = now() - interval '49 hours'
	        WHERE id = $1`, c.s.bookingID)
	base := "/api/v1/bookings/" + c.s.bookingID

	p := expect(t, c.do(http.MethodGet, base+"/return-preview", ""), http.StatusOK, "")
	if p["overdue_units"] != float64(3) || p["late_fee_total"] != float64(300000) || len(p["proposed_lines"].([]any)) != 1 {
		t.Fatalf("preview = %v, want 3 days late, 300000", p)
	}

	photo, other := c.upload("handover_photo"), c.upload("handover_photo")
	keys := `"photo_keys":["` + photo + `"],"meter_value":46000`
	expect(t, c.action(base+"/return", `{`+keys+`}`), http.StatusUnprocessableEntity, "waiver-reason-required")
	expect(t, c.action(base+"/return", `{`+keys+`,"actual_return_at":"2020-01-01T00:00:00Z"}`),
		http.StatusUnprocessableEntity, "validation-failed")
	expect(t, c.action(base+"/return", `{`+keys+`,"confirm_late_fee":true,
		"damages":[{"amount":250000,"description":"Baret pintu","photo_key":"`+other+`"}]}`),
		http.StatusUnprocessableEntity, "validation-failed")

	b := expect(t, c.action(base+"/return", `{`+keys+`,"confirm_late_fee":true,"late_fee_waived":100000,
		"waiver_reason":"Macet karena banjir",
		"damages":[{"amount":250000,"description":"Baret pintu kiri","photo_key":"`+photo+`"}]}`),
		http.StatusOK, "")
	if b["status"] != "returned" || b["actual_return_at"] == nil {
		t.Fatalf("after return: %v / %v", b["status"], b["actual_return_at"])
	}

	w := c.do(http.MethodGet, "/api/v1/invoices?booking_id="+c.s.bookingID, "")
	var invs []map[string]any
	_ = jsonUnmarshal(w.Body.Bytes(), &invs)
	if len(invs) != 2 || invs[1]["number"] != "ISO-0001/2" || invs[1]["total"] != float64(450000) {
		t.Fatalf("invoices = %v, want the rent one plus ISO-0001/2 for 200000 + 250000", invs)
	}
	for _, l := range invs[1]["lines"].([]any) {
		line := l.(map[string]any)
		switch line["kind"] {
		case "late_fee":
			if line["amount"] != float64(200000) || line["waiver_reason"] != "Macet karena banjir" {
				t.Errorf("late_fee line = %v", line)
			}
		case "damage":
			if line["handover_photo_id"] == nil {
				t.Errorf("damage line without its photo: %v", line)
			}
		}
	}
	if n := c.count(`SELECT late_fee_waived FROM handovers WHERE booking_id = $1 AND direction = 'return'`,
		c.s.bookingID); n != 100000 {
		t.Errorf("handover late_fee_waived = %d, want 100000", n)
	}
}

// BR-037: evidence is append-only for every role, and the database is what
// enforces it.
func TestHandoverImmutable(t *testing.T) {
	c := newBookingClient(t)
	expect(t, c.action("/api/v1/bookings/"+c.s.bookingID+"/pickup",
		`{"photo_keys":["`+c.upload("handover_photo")+`"],"meter_value":1}`), http.StatusOK, "")

	var hs []map[string]any
	_ = jsonUnmarshal(c.do(http.MethodGet, "/api/v1/bookings/"+c.s.bookingID+"/handovers", "").Body.Bytes(), &hs)
	id := hs[0]["id"].(string)
	expect(t, c.do(http.MethodPatch, "/api/v1/handovers/"+id, `{"condition_notes":"x"}`), http.StatusMethodNotAllowed, "evidence-immutable")
	expect(t, c.do(http.MethodDelete, "/api/v1/handovers/"+id, ""), http.StatusMethodNotAllowed, "evidence-immutable")

	ctx := t.Context()
	for _, sql := range []string{`UPDATE handovers SET condition_notes = 'x'`, `DELETE FROM handover_photos`} {
		err := c.store.InOwnerTx(owner.NewContext(ctx, c.ownerID), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s as app_user = %v, want permission denied", sql, err)
		}
	}
}

// BR-045 + BR-057: create issues the first invoice -- rent and deposit -- due
// no later than the start.
func TestCreateIssuesFirstInvoice(t *testing.T) {
	c := newBookingClient(t)
	c.exec(`UPDATE resources SET deposit_amount = 500000 WHERE id = $1`, c.s.resourceID)
	b := expect(t, c.book("2027-04-01T09:00:00+07:00", "2027-04-03T09:00:00+07:00"), http.StatusCreated, "")

	var invs []map[string]any
	_ = jsonUnmarshal(c.do(http.MethodGet, "/api/v1/invoices?booking_id="+b["id"].(string), "").Body.Bytes(), &invs)
	if len(invs) != 1 || invs[0]["number"] != b["code"].(string)+"/1" || invs[0]["total"] != float64(1200000) {
		t.Fatalf("first invoice = %v, want %v/1 for 700000 rent + 500000 deposit", invs, b["code"])
	}
	if len(invs[0]["lines"].([]any)) != 2 {
		t.Errorf("lines = %v, want rent and deposit", invs[0]["lines"])
	}
}

// S1-021 + BR-085: the identity photo is stored by key, and every opening
// writes one audit row.
func TestIdentityPhoto(t *testing.T) {
	c := newBookingClient(t)
	path := "/api/v1/customers/" + c.s.customerID + "/identity"
	expect(t, c.do(http.MethodGet, path, ""), http.StatusNotFound, "not-found")

	cust := expect(t, c.do(http.MethodPost, path, `{"object_key":"`+c.upload("identity_photo")+`","id_type":"ktp"}`), http.StatusOK, "")
	if cust["has_id_photo"] != true {
		t.Fatalf("has_id_photo = %v", cust["has_id_photo"])
	}
	for range 2 {
		v := expect(t, c.do(http.MethodGet, path, ""), http.StatusOK, "")
		if v["expires_in"] != float64(300) {
			t.Errorf("expires_in = %v, want 300", v["expires_in"])
		}
	}
	if n := c.count(`SELECT count(*) FROM audit_logs WHERE entity_id = $1 AND action = 'customer.identity.viewed'`,
		c.s.customerID); n != 2 {
		t.Errorf("audit rows = %d, want one per opening (2)", n)
	}
}

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
)

// The acceptance for M2 that the isolation suite cannot reach: S1-020 ..
// S1-027 and S1-078, one test per rule. The fixture is seedBookingOwner -- a
// daily-priced car at 350000, one unit, one customer, and booking ISO-0001
// reserved 3-5 Sep 2026 09:00 WIB.

type bookingClient struct {
	catalogClient
	s       seeded
	store   *db.Store
	ownerID uuid.UUID
}

func newBookingClient(t *testing.T) bookingClient {
	t.Helper()
	ctx := t.Context()
	store := openAppStore(ctx, t)
	ownerID := uuid.Must(uuid.NewV7())
	s := seedBookingOwner(ctx, t, store, ownerID)
	return bookingClient{
		catalogClient: catalogClient{t: t, srv: newServer(t), token: s.accessToken},
		s:             s, store: store, ownerID: ownerID,
	}
}

// exec runs SQL as this rental, for the fixture changes no endpoint offers.
func (c bookingClient) exec(sql string, args ...any) {
	c.t.Helper()
	// WithoutCancel: exec also runs from t.Cleanup, after the test context ends.
	ctx := context.WithoutCancel(c.t.Context())
	if err := c.store.InOwnerTx(owner.NewContext(ctx, c.ownerID), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	}); err != nil {
		c.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (c bookingClient) book(start, end string, extra ...string) *httptest.ResponseRecorder {
	c.t.Helper()
	body := `{"customer_id":"` + c.s.customerID + `","resource_unit_id":"` + c.s.unitID +
		`","start_at":"` + start + `","end_at":"` + end + `"` + strings.Join(extra, "") + `}`
	r := httptest.NewRequest(http.MethodPost, "/api/v1/bookings", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("Idempotency-Key", uuid.Must(uuid.NewV7()).String())
	w := httptest.NewRecorder()
	c.srv.ServeHTTP(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
	return m
}

func expect(t *testing.T, w *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, status, w.Body)
	}
	m := decode(t, w)
	if code != "" {
		if got := path.Base(m["type"].(string)); got != code {
			t.Fatalf("problem = %s, want %s; body=%s", got, code, w.Body)
		}
	}
	return m
}

// BR-022: 3-5 Sep refuses 4-6 Sep and names the booking it collides with;
// ending 09:00 and starting 09:00 do not collide ('[)').
func TestBookingOverlap(t *testing.T) {
	c := newBookingClient(t)

	m := expect(t, c.book("2026-09-04T09:00:00+07:00", "2026-09-06T09:00:00+07:00"),
		http.StatusConflict, "booking-conflict")
	conflicts, _ := m["conflicts"].([]any)
	if len(conflicts) != 1 || conflicts[0].(map[string]any)["code"] != "ISO-0001" {
		t.Fatalf("conflicts = %v, want ISO-0001", m["conflicts"])
	}

	b := expect(t, c.book("2026-09-05T09:00:00+07:00", "2026-09-06T09:00:00+07:00"), http.StatusCreated, "")
	if b["status"] != "reserved" || b["source"] != "staff" {
		t.Errorf("staff booking = %v/%v, want reserved/staff", b["status"], b["source"])
	}
}

// BR-024: codes come from the counter with the prefix read at that moment;
// changing the prefix rewrites nothing and resets nothing.
func TestBookingCodes(t *testing.T) {
	c := newBookingClient(t)
	first := expect(t, c.book("2026-10-01T09:00:00+07:00", "2026-10-02T09:00:00+07:00"), http.StatusCreated, "")
	if first["code"] != "SWN-0001" {
		t.Fatalf("first code = %v, want SWN-0001", first["code"])
	}

	c.exec(`UPDATE owners SET booking_code_prefix = 'RB' WHERE id = $1`, c.ownerID)
	second := expect(t, c.book("2026-10-03T09:00:00+07:00", "2026-10-04T09:00:00+07:00"), http.StatusCreated, "")
	if second["code"] != "RB-0002" {
		t.Fatalf("second code = %v, want RB-0002 -- counter continues, prefix is today's", second["code"])
	}
	again := expect(t, c.do(http.MethodGet, "/api/v1/bookings/"+first["id"].(string), ""), http.StatusOK, "")
	if again["code"] != "SWN-0001" {
		t.Errorf("old code became %v; a prefix change must not rewrite it", again["code"])
	}
}

// BR-014, BR-015, BR-016: the snapshot is the server's. A sent price is
// refused, a sent end_at_with_buffer is overwritten by the trigger, and NULL
// nominals copy as NULL.
func TestBookingSnapshot(t *testing.T) {
	c := newBookingClient(t)

	expect(t, c.book("2026-10-01T09:00:00+07:00", "2026-10-02T09:00:00+07:00", `,"unit_price":1`),
		http.StatusUnprocessableEntity, "validation-failed")

	c.exec(`UPDATE resources SET buffer_minutes = 120 WHERE id = $1`, c.s.resourceID)
	b := expect(t, c.book("2026-10-01T08:00:00+07:00", "2026-10-01T10:00:00+07:00",
		`,"end_at_with_buffer":"2030-01-01T00:00:00Z"`), http.StatusCreated, "")
	if got, _ := time.Parse(time.RFC3339, b["end_at_with_buffer"].(string)); !got.Equal(
		time.Date(2026, 10, 1, 5, 0, 0, 0, time.UTC)) {
		t.Errorf("end_at_with_buffer = %v, want 10:00 WIB + 120 min = 05:00Z", b["end_at_with_buffer"])
	}
	if b["buffer_minutes"] != float64(120) || b["unit_price"] != float64(350000) ||
		b["duration_qty"] != float64(1) || b["subtotal"] != float64(350000) {
		t.Errorf("snapshot = %v", b)
	}
	if b["deposit_amount"] != nil || b["late_fee_per_unit"] != nil {
		t.Errorf("NULL nominals must copy as NULL, got %v / %v", b["deposit_amount"], b["late_fee_per_unit"])
	}

	// A later price change never reaches it, and the resource says how many
	// running bookings kept the old price.
	upd := expect(t, c.do(http.MethodPatch, "/api/v1/resources/"+c.s.resourceID, `{"base_price":999000}`),
		http.StatusOK, "")
	if upd["active_bookings"] != float64(2) {
		t.Errorf("active_bookings = %v, want 2", upd["active_bookings"])
	}
	again := expect(t, c.do(http.MethodGet, "/api/v1/bookings/"+b["id"].(string), ""), http.StatusOK, "")
	if again["unit_price"] != float64(350000) {
		t.Errorf("unit_price became %v after a price change", again["unit_price"])
	}
}

// BR-015 + BR-020: a 120-minute buffer moves "available" from 10:00 to 12:00,
// and a maintenance unit disappears (BR-013).
func TestAvailability(t *testing.T) {
	c := newBookingClient(t)
	c.exec(`UPDATE resources SET buffer_minutes = 120 WHERE id = $1`, c.s.resourceID)
	expect(t, c.book("2026-10-01T08:00:00+07:00", "2026-10-01T10:00:00+07:00"), http.StatusCreated, "")
	// The same buffer again, for the requested range: 0 so this test reads
	// only the existing booking's.
	c.exec(`UPDATE resources SET buffer_minutes = 0 WHERE id = $1`, c.s.resourceID)

	units := func(start, end string) int {
		m := expect(t, c.do(http.MethodGet, "/api/v1/availability?resource_id="+c.s.resourceID+
			"&start_at="+strings.ReplaceAll(start, "+", "%2B")+"&end_at="+strings.ReplaceAll(end, "+", "%2B"), ""),
			http.StatusOK, "")
		data := m["data"].([]any)
		if len(data) != 1 {
			t.Fatalf("data = %v, want the one resource", data)
		}
		return len(data[0].(map[string]any)["available_units"].([]any))
	}
	if n := units("2026-10-01T11:00:00+07:00", "2026-10-01T13:00:00+07:00"); n != 0 {
		t.Errorf("11:00 inside the buffer: %d units, want 0", n)
	}
	if n := units("2026-10-01T12:00:00+07:00", "2026-10-01T14:00:00+07:00"); n != 1 {
		t.Errorf("12:00 after the buffer: %d units, want 1", n)
	}
	c.exec(`UPDATE resource_units SET status = 'maintenance' WHERE id = $1`, c.s.unitID)
	if n := units("2026-12-01T12:00:00+07:00", "2026-12-02T12:00:00+07:00"); n != 0 {
		t.Errorf("maintenance unit offered: %d units", n)
	}
}

// BR-021 + BR-016: an empty bound is no bound; a set one is enforced.
func TestBookingDuration(t *testing.T) {
	c := newBookingClient(t)
	expect(t, c.book("2026-10-01T09:00:00+07:00", "2026-10-31T09:00:00+07:00"), http.StatusCreated, "")

	c.exec(`UPDATE resources SET min_duration = 2 WHERE id = $1`, c.s.resourceID)
	expect(t, c.book("2026-11-01T09:00:00+07:00", "2026-11-02T09:00:00+07:00"),
		http.StatusUnprocessableEntity, "duration-out-of-range")
}

// BR-028: only the owner blocks, a block needs a reason, a blocked customer
// gets no new booking, and the act is audited.
func TestBlacklist(t *testing.T) {
	c := newBookingClient(t)
	bl := "/api/v1/customers/" + c.s.customerID + "/blacklist"

	expect(t, c.do(http.MethodPost, bl, `{"reason":"  "}`), http.StatusUnprocessableEntity, "validation-failed")
	cust := expect(t, c.do(http.MethodPost, bl, `{"reason":"unit kembali penyok"}`), http.StatusOK, "")
	if cust["is_blacklisted"] != true || cust["blacklist_reason"] != "unit kembali penyok" {
		t.Fatalf("after block: %v", cust)
	}
	expect(t, c.book("2026-10-01T09:00:00+07:00", "2026-10-02T09:00:00+07:00"),
		http.StatusUnprocessableEntity, "customer-blacklisted")

	// PATCH cannot touch it -- an operator may call PATCH.
	expect(t, c.do(http.MethodPatch, "/api/v1/customers/"+c.s.customerID, `{"is_blacklisted":false}`),
		http.StatusUnprocessableEntity, "validation-failed")

	var audits int
	ctx := t.Context()
	if err := c.store.InOwnerTx(owner.NewContext(ctx, c.ownerID), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE entity_id = $1
		                          AND action = 'customer.blacklisted'`, c.s.customerID).Scan(&audits)
	}); err != nil || audits != 1 {
		t.Fatalf("audit rows = %d, %v; want 1", audits, err)
	}

	open := expect(t, c.do(http.MethodDelete, bl, ""), http.StatusOK, "")
	if open["is_blacklisted"] != false || open["blacklist_reason"] != nil {
		t.Errorf("after unblock: %v", open)
	}

	// The operator: may edit the customer, may not block.
	store := openAppStore(ctx, t)
	opOwner := uuid.Must(uuid.NewV7())
	op := seedSignedInUser(ctx, t, store, opOwner)
	w := catalogClient{t: t, srv: c.srv, token: op.accessToken}.
		do(http.MethodPost, "/api/v1/customers/"+uuid.Must(uuid.NewV7()).String()+"/blacklist", `{"reason":"x"}`)
	m := expect(t, w, http.StatusForbidden, "permission-denied")
	if !strings.Contains(m["detail"].(string), "customers:blacklist") {
		t.Errorf("403 must name the permission, got %q", m["detail"])
	}
}

// BR-085: the identity number is encrypted at rest, never returned, and only
// its last four digits come back. audit_logs refuses UPDATE for app_user.
func TestCustomerIdentity(t *testing.T) {
	c := newBookingClient(t)
	const number = "3174012345670001"

	expect(t, c.do(http.MethodPost, "/api/v1/customers",
		`{"name":"Sari","phone":"081311112222","id_number":"`+number+`"}`),
		http.StatusUnprocessableEntity, "validation-failed")

	w := c.do(http.MethodPost, "/api/v1/customers",
		`{"name":"Sari","phone":"081311112222","id_type":"ktp","id_number":"`+number+`"}`)
	cust := expect(t, w, http.StatusCreated, "")
	if strings.Contains(w.Body.String(), number) || cust["id_number_last4"] != "0001" {
		t.Fatalf("response leaks or lacks last4: %s", w.Body)
	}

	ctx := t.Context()
	var enc []byte
	if err := c.store.InOwnerTx(owner.NewContext(ctx, c.ownerID), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id_number_enc FROM customers WHERE id = $1`, cust["id"]).Scan(&enc)
	}); err != nil {
		t.Fatal(err)
	}
	if len(enc) == 0 || bytes.Contains(enc, []byte(number)) {
		t.Fatalf("id_number_enc is empty or plaintext: %q", enc)
	}

	err := c.store.InOwnerTx(owner.NewContext(ctx, c.ownerID), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE audit_logs SET action = 'x'`)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("UPDATE audit_logs as app_user = %v, want permission denied", err)
	}
}

// BR-029: swap only while reserved, only within the resource, and the target
// must be free.
func TestSwapUnit(t *testing.T) {
	c := newBookingClient(t)
	other, foreign, foreignRes := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	c.exec(`INSERT INTO resource_units (id, owner_id, resource_id, code) VALUES ($1, $2, $3, $4)`,
		other, c.ownerID, c.s.resourceID, "SWAP-"+other.String())
	c.exec(`INSERT INTO resources (id, owner_id, name, pricing_unit, base_price) VALUES ($1, $2, 'Lain', 'day', 1)`,
		foreignRes, c.ownerID)
	c.exec(`INSERT INTO resource_units (id, owner_id, resource_id, code) VALUES ($1, $2, $3, $4)`,
		foreign, c.ownerID, foreignRes, "FOREIGN-"+foreign.String())
	t.Cleanup(func() {
		c.exec(`DELETE FROM invoice_lines WHERE owner_id = $1`, c.ownerID)
		c.exec(`DELETE FROM invoices WHERE owner_id = $1`, c.ownerID)
		c.exec(`DELETE FROM bookings WHERE owner_id = $1`, c.ownerID)
		c.exec(`DELETE FROM resource_units WHERE id IN ($1, $2)`, other, foreign)
		c.exec(`DELETE FROM resources WHERE id = $1`, foreignRes)
	})

	bk := "/api/v1/bookings/" + c.s.bookingID
	expect(t, c.do(http.MethodPatch, bk, `{"resource_unit_id":"`+foreign.String()+`"}`),
		http.StatusUnprocessableEntity, "validation-failed")

	// Occupy the other unit on an overlapping range, so the swap collides.
	c.exec(`INSERT INTO bookings (id, owner_id, code, customer_id, resource_id, resource_unit_id,
	         start_at, end_at, status, unit_price, pricing_unit, duration_qty, subtotal)
	        VALUES ($1, $2, 'ISO-0002', $3, $4, $5, '2026-09-04 09:00+07', '2026-09-06 09:00+07',
	                'reserved', 1, 'day', 1, 1)`,
		uuid.Must(uuid.NewV7()), c.ownerID, c.s.customerID, c.s.resourceID, other)
	expect(t, c.do(http.MethodPatch, bk, `{"resource_unit_id":"`+other.String()+`"}`),
		http.StatusConflict, "booking-conflict")
	c.exec(`UPDATE bookings SET status = 'cancelled', cancelled_reason = 'manual' WHERE code = 'ISO-0002'`)

	moved := expect(t, c.do(http.MethodPatch, bk, `{"resource_unit_id":"`+other.String()+`"}`), http.StatusOK, "")
	if moved["unit"].(map[string]any)["id"] != other.String() {
		t.Fatalf("swap did not move the booking: %v", moved["unit"])
	}

	c.exec(`UPDATE bookings SET status = 'picked_up' WHERE id = $1`, c.s.bookingID)
	expect(t, c.do(http.MethodPatch, bk, `{"resource_unit_id":"`+c.s.unitID+`"}`),
		http.StatusConflict, "unit-not-swappable")
}

// BR-026 + BR-023: confirm re-runs the conflict check; cancel frees the unit.
func TestConfirmAndCancel(t *testing.T) {
	c := newBookingClient(t)
	draft := uuid.Must(uuid.NewV7())
	c.exec(`INSERT INTO bookings (id, owner_id, code, customer_id, resource_id, resource_unit_id,
	         start_at, end_at, status, source, unit_price, pricing_unit, duration_qty, subtotal)
	        VALUES ($1, $2, 'ISO-D1', $3, $4, $5, '2026-09-04 09:00+07', '2026-09-06 09:00+07',
	                'draft', 'public_page', 1, 'day', 2, 2)`,
		draft, c.ownerID, c.s.customerID, c.s.resourceID, c.s.unitID)

	expect(t, c.do(http.MethodPost, "/api/v1/bookings/"+draft.String()+"/confirm", ""),
		http.StatusConflict, "booking-conflict")
	expect(t, c.do(http.MethodPost, "/api/v1/bookings/"+c.s.bookingID+"/confirm", ""),
		http.StatusUnprocessableEntity, "validation-failed")

	cancelled := expect(t, c.do(http.MethodPost, "/api/v1/bookings/"+c.s.bookingID+"/cancel", ""), http.StatusOK, "")
	if cancelled["status"] != "cancelled" || cancelled["cancelled_reason"] != "manual" {
		t.Fatalf("after cancel: %v / %v", cancelled["status"], cancelled["cancelled_reason"])
	}
	confirmed := expect(t, c.do(http.MethodPost, "/api/v1/bookings/"+draft.String()+"/confirm", ""), http.StatusOK, "")
	if confirmed["status"] != "reserved" {
		t.Fatalf("draft after the blocker was cancelled: %v", confirmed["status"])
	}
	expect(t, c.do(http.MethodPost, "/api/v1/bookings/"+c.s.bookingID+"/cancel", ""),
		http.StatusUnprocessableEntity, "validation-failed")
}

// BR-041 + BR-033: overdue is a derived filter, the calendar shows it as a
// state, draft never appears, and maintenance is one segment.
func TestListAndCalendar(t *testing.T) {
	c := newBookingClient(t)
	late := uuid.Must(uuid.NewV7())
	c.exec(`UPDATE bookings SET status = 'picked_up' WHERE id = $1`, c.s.bookingID)
	c.exec(`INSERT INTO bookings (id, owner_id, code, customer_id, resource_id, resource_unit_id,
	         start_at, end_at, status, unit_price, pricing_unit, duration_qty, subtotal)
	        VALUES ($1, $2, 'ISO-D2', $3, $4, $5, '2026-09-10 09:00+07', '2026-09-12 09:00+07',
	                'draft', 1, 'day', 2, 2)`,
		late, c.ownerID, c.s.customerID, c.s.resourceID, c.s.unitID)

	over := expect(t, c.do(http.MethodGet, "/api/v1/bookings?overdue=true", ""), http.StatusOK, "")
	data := over["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["code"] != "ISO-0001" || data[0].(map[string]any)["overdue"] != true {
		t.Fatalf("overdue filter = %v", data)
	}
	page := expect(t, c.do(http.MethodGet, "/api/v1/bookings?limit=1", ""), http.StatusOK, "")
	if len(page["data"].([]any)) != 1 || page["next_cursor"] == nil {
		t.Fatalf("limit=1 page = %v", page)
	}
	rest := expect(t, c.do(http.MethodGet, "/api/v1/bookings?limit=1&cursor="+page["next_cursor"].(string), ""),
		http.StatusOK, "")
	if len(rest["data"].([]any)) != 1 || rest["next_cursor"] != nil {
		t.Fatalf("second page = %v", rest)
	}

	cal := expect(t, c.do(http.MethodGet,
		"/api/v1/calendar?from=2026-09-01T00:00:00%2B07:00&to=2026-09-15T00:00:00%2B07:00", ""), http.StatusOK, "")
	rows := cal["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("lanes = %d, want 1", len(rows))
	}
	var states []string
	for _, s := range rows[0].(map[string]any)["segments"].([]any) {
		states = append(states, s.(map[string]any)["state"].(string))
	}
	if strings.Join(states, ",") != "available,overdue,available" {
		t.Errorf("states = %v, want available,overdue,available -- the draft must not render", states)
	}

	c.exec(`UPDATE resource_units SET status = 'maintenance' WHERE id = $1`, c.s.unitID)
	cal = expect(t, c.do(http.MethodGet,
		"/api/v1/calendar?from=2026-09-01T00:00:00%2B07:00&to=2026-09-15T00:00:00%2B07:00", ""), http.StatusOK, "")
	segs := cal["data"].([]any)[0].(map[string]any)["segments"].([]any)
	if len(segs) != 1 || segs[0].(map[string]any)["state"] != "maintenance" {
		t.Errorf("maintenance lane = %v", segs)
	}
	c.exec(`UPDATE resource_units SET status = 'retired' WHERE id = $1`, c.s.unitID)
	cal = expect(t, c.do(http.MethodGet,
		"/api/v1/calendar?from=2026-09-01T00:00:00%2B07:00&to=2026-09-15T00:00:00%2B07:00", ""), http.StatusOK, "")
	if n := len(cal["data"].([]any)); n != 0 {
		t.Errorf("retired unit has %d lanes, want none", n)
	}
}

// BR-013: a unit going to maintenance lists what it does not cancel.
func TestUnitMaintenanceListsAffectedBookings(t *testing.T) {
	c := newBookingClient(t)
	c.exec(`UPDATE bookings SET start_at = now() + interval '1 day', end_at = now() + interval '3 days'
	         WHERE id = $1`, c.s.bookingID)
	m := expect(t, c.do(http.MethodPatch, "/api/v1/units/"+c.s.unitID, `{"status":"maintenance"}`), http.StatusOK, "")
	affected := m["warning"].(map[string]any)["affected_bookings"].([]any)
	if len(affected) != 1 || affected[0].(map[string]any)["code"] != "ISO-0001" {
		t.Fatalf("affected_bookings = %v", affected)
	}
}

// GET /bookings: repeated unit_id/customer_id/resource_id are "any of these";
// code is a case-insensitive substring, and a typed '%' is a character, not a
// wildcard.
func TestListBookingFilters(t *testing.T) {
	c := newBookingClient(t)
	cust, res, unit, bk := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	c.exec(`INSERT INTO customers (id, owner_id, name, phone) VALUES ($1, $2, 'Andi', '081200000000')`, cust, c.ownerID)
	c.exec(`INSERT INTO resources (id, owner_id, name, pricing_unit, base_price) VALUES ($1, $2, 'BYD M6', 'day', 1)`, res, c.ownerID)
	c.exec(`INSERT INTO resource_units (id, owner_id, resource_id, code) VALUES ($1, $2, $3, 'B1122KZX')`, unit, c.ownerID, res)
	c.exec(`INSERT INTO bookings (id, owner_id, code, customer_id, resource_id, resource_unit_id,
	         start_at, end_at, status, unit_price, pricing_unit, duration_qty, subtotal)
	        VALUES ($1, $2, 'ISO-0777', $3, $4, $5, '2026-09-20 09:00+07', '2026-09-21 09:00+07',
	                'reserved', 1, 'day', 1, 1)`, bk, c.ownerID, cust, res, unit)

	codes := func(query string) string {
		t.Helper()
		page := expect(t, c.do(http.MethodGet, "/api/v1/bookings?"+query, ""), http.StatusOK, "")
		var out []string
		for _, b := range page["data"].([]any) {
			out = append(out, b.(map[string]any)["code"].(string))
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	for _, tc := range []struct{ query, want string }{
		{"code=0777", "ISO-0777"},
		{"code=iso-0001", "ISO-0001"},
		{"code=%25", ""},
		{"code=%20%20", "ISO-0001,ISO-0777"}, // blank is "any", not "match nothing"
		{"unit_id=" + unit.String(), "ISO-0777"},
		{"unit_id=" + unit.String() + "&unit_id=" + c.s.unitID, "ISO-0001,ISO-0777"},
		{"customer_id=" + cust.String(), "ISO-0777"},
		{"customer_id=" + cust.String() + "&customer_id=" + c.s.customerID, "ISO-0001,ISO-0777"},
		{"resource_id=" + res.String(), "ISO-0777"},
		{"resource_id=" + c.s.resourceID + "&code=0777", ""},
	} {
		if got := codes(tc.query); got != tc.want {
			t.Errorf("?%s = %q, want %q", tc.query, got, tc.want)
		}
	}
}

// GET /customers?blacklisted= narrows to one side; absent means both.
func TestListCustomersBlacklisted(t *testing.T) {
	c := newBookingClient(t)
	blocked := uuid.Must(uuid.NewV7())
	c.exec(`INSERT INTO customers (id, owner_id, name, phone, is_blacklisted, blacklist_reason)
	        VALUES ($1, $2, 'Diblokir', '081299999999', true, 'tidak membayar')`, blocked, c.ownerID)

	ids := func(query string) string {
		t.Helper()
		page := expect(t, c.do(http.MethodGet, "/api/v1/customers"+query, ""), http.StatusOK, "")
		var out []string
		for _, x := range page["data"].([]any) {
			out = append(out, x.(map[string]any)["id"].(string))
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	both := []string{blocked.String(), c.s.customerID}
	sort.Strings(both)
	for _, tc := range []struct{ query, want string }{
		{"?blacklisted=true", blocked.String()},
		{"?blacklisted=false", c.s.customerID},
		{"", strings.Join(both, ",")},
	} {
		if got := ids(tc.query); got != tc.want {
			t.Errorf("%q = %s, want %s", tc.query, got, tc.want)
		}
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

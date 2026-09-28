package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The acceptance for S1-014 .. S1-017 that the isolation suite cannot reach.
//
// The isolation suite proves one rental cannot see another's catalogue. What is
// below proves the catalogue is CORRECT for the rental that owns it -- and
// almost every case here is a line in BR-016, BR-011, BR-012 or BR-013 that
// would otherwise be a comment nobody re-reads.

// catalogClient is the small amount of plumbing every case here wants: a signed
// in owner, and a way to make one JSON request as them.
type catalogClient struct {
	t     *testing.T
	srv   http.Handler
	token string
}

func newCatalogClient(t *testing.T) catalogClient {
	t.Helper()
	ctx := t.Context()
	store := openAppStore(ctx, t)
	// Owner role, not operator: an operator is refused at requirePermission and
	// every case below would go green without a handler ever running.
	s := seedSignedInOwner(ctx, t, store, uuid.Must(uuid.NewV7()))
	return catalogClient{t: t, srv: newServer(t), token: s.accessToken}
}

func (c catalogClient) do(method, path, body string) *httptest.ResponseRecorder {
	c.t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	w := httptest.NewRecorder()
	c.srv.ServeHTTP(w, r)
	return w
}

// resourceBodyShape is what the tests read back. Pointers throughout, because
// the whole of BR-016 is about telling null from a number.
type resourceBodyShape struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	PricingUnit    string `json:"pricing_unit"`
	BasePrice      int64  `json:"base_price"`
	DepositAmount  *int64 `json:"deposit_amount"`
	LateFeePerUnit *int64 `json:"late_fee_per_unit"`
	MinDuration    *int   `json:"min_duration"`
	MaxDuration    *int   `json:"max_duration"`
	BufferMinutes  int    `json:"buffer_minutes"`
	Status         string `json:"status"`
	UnitCount      int    `json:"unit_count"`
	ActiveBookings int    `json:"active_bookings"`
}

func (c catalogClient) createResource(body string) resourceBodyShape {
	c.t.Helper()
	w := c.do(http.MethodPost, "/api/v1/resources", body)
	if w.Code != http.StatusCreated {
		c.t.Fatalf("create resource: status = %d, want 201\nbody: %s", w.Code, w.Body.String())
	}
	var out resourceBodyShape
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		c.t.Fatalf("decode resource: %v", err)
	}
	return out
}

func problemOf(t *testing.T, w *httptest.ResponseRecorder) (code, detail string, fields []string) {
	t.Helper()
	var problem struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
		Errors []struct {
			Field string `json:"field"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v\nbody: %s", err, w.Body.String())
	}
	for _, e := range problem.Errors {
		fields = append(fields, e.Field)
	}
	parts := strings.Split(problem.Type, "/")
	return parts[len(parts)-1], problem.Detail, fields
}

// TestEmptyNominalsStayEmpty is BR-016's first half, and the one a screen gets
// wrong by accident: omitting a field must not quietly become zero.
//
// A resource saved without deposit, late fee or duration bounds runs WITHOUT
// them -- BR-045 issues no deposit line, BR-046 issues no late fee, BR-021
// imposes no bounds. Zero would mean all three rules apply and happen to be
// worth nothing, which is a different rental business.
func TestEmptyNominalsStayEmpty(t *testing.T) {
	c := newCatalogClient(t)
	got := c.createResource(`{"name":"Tenda Dome 4 Orang","base_price":75000}`)

	for _, f := range []struct {
		name string
		got  any
	}{
		{"deposit_amount", got.DepositAmount},
		{"late_fee_per_unit", got.LateFeePerUnit},
		{"min_duration", got.MinDuration},
		{"max_duration", got.MaxDuration},
	} {
		if !isNilPtr(f.got) {
			t.Errorf("%s = %v, want null -- an omitted nominal must not become 0 (BR-016)", f.name, f.got)
		}
	}
	// buffer_minutes is the deliberate exception (BR-015): NOT NULL, default 0,
	// because end_at + NULL is an unbounded range that collides with every
	// future booking the unit has.
	if got.BufferMinutes != 0 {
		t.Errorf("buffer_minutes = %d, want 0", got.BufferMinutes)
	}
}

func isNilPtr(v any) bool {
	switch p := v.(type) {
	case *int64:
		return p == nil
	case *int:
		return p == nil
	}
	return false
}

// TestZeroNominalIsRefused is BR-016's second half.
//
// 0 and null must never be two ways to write one state, so the database refuses
// 0 outright. The message matters as much as the status: PRD A1 asks the system
// to tell the owner to LEAVE IT EMPTY. "Must be greater than zero" invites them
// to type 1, which is a real deposit of one rupiah.
func TestZeroNominalIsRefused(t *testing.T) {
	c := newCatalogClient(t)

	for _, field := range []string{
		"deposit_amount", "late_fee_per_unit", "min_duration", "max_duration",
	} {
		t.Run(field, func(t *testing.T) {
			w := c.do(http.MethodPost, "/api/v1/resources",
				`{"name":"Nol","base_price":1000,"`+field+`":0}`)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422\nbody: %s", w.Code, w.Body.String())
			}
			code, detail, fields := problemOf(t, w)
			if code != "validation-failed" {
				t.Errorf("code = %q, want validation-failed", code)
			}
			if !strings.Contains(strings.ToLower(detail), "empty") {
				t.Errorf("detail %q does not tell the owner to leave the field empty (PRD A1)", detail)
			}
			if len(fields) == 0 || fields[0] != field {
				t.Errorf("errors[].field = %v, want [%s] so the message lands on the input", fields, field)
			}
		})
	}
}

// TestDurationOrderIsRefused: a maximum shorter than the minimum is a resource
// nobody can ever book, and the database says so rather than the handler.
func TestDurationOrderIsRefused(t *testing.T) {
	c := newCatalogClient(t)
	w := c.do(http.MethodPost, "/api/v1/resources",
		`{"name":"Terbalik","base_price":1000,"min_duration":7,"max_duration":3}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422\nbody: %s", w.Code, w.Body.String())
	}

	// One-sided bounds are fine and must stay fine: "at least 7 days, no upper
	// limit" is an ordinary rental.
	got := c.createResource(`{"name":"Sebelah","base_price":1000,"min_duration":7}`)
	if got.MinDuration == nil || *got.MinDuration != 7 || got.MaxDuration != nil {
		t.Errorf("one-sided bound not preserved: min=%v max=%v", got.MinDuration, got.MaxDuration)
	}
}

// TestPricingUnitComesFromThePreset is BR-017 rule 1.
//
// The fixture's rental is vehicle_rental, whose unit is `day`. Nothing in the
// request said so -- and nothing could, because pricing_unit is not in
// ResourceCreate at all, so the generated type has no field for it. A client
// that tries is a compile error rather than a 422.
func TestPricingUnitComesFromThePreset(t *testing.T) {
	c := newCatalogClient(t)
	got := c.createResource(`{"name":"Avanza 2021","base_price":350000}`)
	if got.PricingUnit != "day" {
		t.Errorf("pricing_unit = %q, want day from the owner's preset (BR-012, BR-017)", got.PricingUnit)
	}

	// A body that names it anyway is REFUSED, not quietly ignored
	// (04-api-spec.md section 3.2). Dropping it silently is the worse
	// failure: a caller that believed it chose the unit gets an invoice
	// computed on another one, and nothing in the exchange said no.
	for _, tc := range []struct{ name, body, field string }{
		{"pricing_unit", `{"name":"Lapangan","base_price":100000,"pricing_unit":"hour"}`, "pricing_unit"},
		{"unit_count", `{"name":"Lapangan","base_price":100000,"unit_count":9}`, "unit_count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := c.do(http.MethodPost, "/api/v1/resources", tc.body)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422\nbody: %s", w.Code, w.Body.String())
			}
			_, _, fields := problemOf(t, w)
			if len(fields) == 0 || fields[0] != tc.field {
				t.Errorf("errors[].field = %v, want [%s]", fields, tc.field)
			}
		})
	}

	// And on PATCH too: the field is server-owned everywhere, not only at
	// creation time.
	if w := c.do(http.MethodPatch, "/api/v1/resources/"+got.ID,
		`{"pricing_unit":"hour"}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("PATCH pricing_unit: status = %d, want 422\nbody: %s", w.Code, w.Body.String())
	}
}

// TestPatchRevokesOnlyWhatItNames is the distinction the whole CASE/COALESCE
// split in db/queries/catalog.sql exists for.
//
// Sending `"deposit_amount": null` revokes the deposit. NOT sending the key
// leaves it alone. The generated ResourceUpdate has *int64 for both, so the
// handler reads the raw keys alongside the decoded body -- if that ever gets
// simplified away, this test is what goes red.
func TestPatchRevokesOnlyWhatItNames(t *testing.T) {
	c := newCatalogClient(t)
	got := c.createResource(
		`{"name":"Avanza 2021","base_price":350000,"deposit_amount":500000,"late_fee_per_unit":50000}`)
	if got.DepositAmount == nil || *got.DepositAmount != 500000 {
		t.Fatalf("seeded deposit = %v, want 500000", got.DepositAmount)
	}

	// Touch something else entirely. Both nominals must survive.
	w := c.do(http.MethodPatch, "/api/v1/resources/"+got.ID, `{"name":"Avanza 2021 Putih"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch name: status = %d\nbody: %s", w.Code, w.Body.String())
	}
	var after resourceBodyShape
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if after.DepositAmount == nil || *after.DepositAmount != 500000 {
		t.Errorf("deposit_amount = %v after an unrelated PATCH, want 500000 -- "+
			"an absent key leaves the value alone", after.DepositAmount)
	}

	// Now revoke exactly one of them.
	w = c.do(http.MethodPatch, "/api/v1/resources/"+got.ID, `{"deposit_amount":null}`)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke deposit: status = %d\nbody: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if after.DepositAmount != nil {
		t.Errorf("deposit_amount = %v, want null -- explicit null revokes (BR-016)", *after.DepositAmount)
	}
	if after.LateFeePerUnit == nil || *after.LateFeePerUnit != 50000 {
		t.Errorf("late_fee_per_unit = %v, want 50000 -- revoking one must not revoke its neighbour",
			after.LateFeePerUnit)
	}
}

// TestPatchReportsActiveBookings is BR-014's visible half.
//
// A price change never reaches a booking already made, and the response says
// how many are in that position so the owner does not have to guess. The count
// is 0 until S1-022 creates the bookings table; the field being present and
// correct-shaped now is what lets S1-018's screen be written once.
func TestPatchReportsActiveBookings(t *testing.T) {
	c := newCatalogClient(t)
	got := c.createResource(`{"name":"Avanza 2021","base_price":350000}`)

	w := c.do(http.MethodPatch, "/api/v1/resources/"+got.ID, `{"base_price":400000}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d\nbody: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["active_bookings"]; !ok {
		t.Error("response has no active_bookings -- BR-014 wants the count, not a guess")
	}
}

// TestUnitCodeIsUniquePerRental is BR-011.
func TestUnitCodeIsUniquePerRental(t *testing.T) {
	c := newCatalogClient(t)
	res := c.createResource(`{"name":"Avanza 2021","base_price":350000}`)
	units := "/api/v1/resources/" + res.ID + "/units"

	if w := c.do(http.MethodPost, units, `{"code":"B 1234 XY","label":"Avanza Putih"}`); w.Code != http.StatusCreated {
		t.Fatalf("first unit: status = %d\nbody: %s", w.Code, w.Body.String())
	}

	w := c.do(http.MethodPost, units, `{"code":"B 1234 XY"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate code: status = %d, want 422\nbody: %s", w.Code, w.Body.String())
	}
	_, _, fields := problemOf(t, w)
	if len(fields) == 0 || fields[0] != "code" {
		t.Errorf("errors[].field = %v, want [code] -- the message belongs on the input "+
			"the person typed the plate into", fields)
	}
}

// TestDeletedUnitReleasesItsCode: the unique index ignores deleted rows, and
// that is load-bearing rather than incidental. A plate that moves to another
// car has to be usable on that car.
func TestDeletedUnitReleasesItsCode(t *testing.T) {
	c := newCatalogClient(t)
	res := c.createResource(`{"name":"Avanza 2021","base_price":350000}`)
	units := "/api/v1/resources/" + res.ID + "/units"

	w := c.do(http.MethodPost, units, `{"code":"B 5678 ZZ"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d\nbody: %s", w.Code, w.Body.String())
	}
	var unit struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &unit); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if w := c.do(http.MethodDelete, "/api/v1/units/"+unit.ID, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d\nbody: %s", w.Code, w.Body.String())
	}
	if w := c.do(http.MethodPost, units, `{"code":"B 5678 ZZ"}`); w.Code != http.StatusCreated {
		t.Fatalf("reuse after delete: status = %d, want 201\nbody: %s", w.Code, w.Body.String())
	}
}

// TestMaintenanceWarnsAndKeeps is BR-013, and it is a test about what does NOT
// happen: the unit changes status, nothing is cancelled, nothing is deleted,
// and the response carries the list the owner decides from.
//
// affected_bookings is empty until S1-022. Its presence is the contract.
func TestMaintenanceWarnsAndKeeps(t *testing.T) {
	c := newCatalogClient(t)
	res := c.createResource(`{"name":"Avanza 2021","base_price":350000}`)
	units := "/api/v1/resources/" + res.ID + "/units"

	w := c.do(http.MethodPost, units, `{"code":"B 9999 AA"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create unit: status = %d\nbody: %s", w.Code, w.Body.String())
	}
	var unit struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &unit); err != nil {
		t.Fatalf("decode: %v", err)
	}

	w = c.do(http.MethodPatch, "/api/v1/units/"+unit.ID, `{"status":"maintenance"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 -- maintenance warns, it does not refuse\nbody: %s",
			w.Code, w.Body.String())
	}
	var updated struct {
		Status  string `json:"status"`
		Warning *struct {
			AffectedBookings []any `json:"affected_bookings"`
		} `json:"warning"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.Status != "maintenance" {
		t.Errorf("status = %q, want maintenance", updated.Status)
	}
	if updated.Warning == nil || updated.Warning.AffectedBookings == nil {
		t.Error("no warning.affected_bookings -- BR-013 wants the list the owner decides from")
	}

	// And the row is still there, with its status changed rather than removed.
	w = c.do(http.MethodGet, units, "")
	if !strings.Contains(w.Body.String(), "B 9999 AA") {
		t.Errorf("unit vanished from its resource after maintenance\nbody: %s", w.Body.String())
	}
}

// TestForeignStatusIsRefusedByTheDatabase: the CHECK added in 000007/000008 is
// what stops a status nobody defined from silently removing a unit from
// availability search (BR-013).
func TestForeignStatusIsRefusedByTheDatabase(t *testing.T) {
	c := newCatalogClient(t)
	res := c.createResource(`{"name":"Avanza 2021","base_price":350000}`)

	if w := c.do(http.MethodPatch, "/api/v1/resources/"+res.ID,
		`{"status":"draft"}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("resource status draft: status = %d, want 422\nbody: %s", w.Code, w.Body.String())
	}

	w := c.do(http.MethodPost, "/api/v1/resources/"+res.ID+"/units", `{"code":"B 1 AA"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create unit: status = %d\nbody: %s", w.Code, w.Body.String())
	}
	var unit struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &unit); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if w := c.do(http.MethodPatch, "/api/v1/units/"+unit.ID,
		`{"status":"rusak"}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("unit status rusak: status = %d, want 422\nbody: %s", w.Code, w.Body.String())
	}
}

// TestDeletingAResourceReleasesItsUnits: units left behind would belong to a
// kind nothing can see, and would go on holding their codes.
func TestDeletingAResourceReleasesItsUnits(t *testing.T) {
	c := newCatalogClient(t)
	first := c.createResource(`{"name":"Avanza 2021","base_price":350000}`)
	if w := c.do(http.MethodPost, "/api/v1/resources/"+first.ID+"/units",
		`{"code":"B 7777 CC"}`); w.Code != http.StatusCreated {
		t.Fatalf("create unit: status = %d\nbody: %s", w.Code, w.Body.String())
	}

	if w := c.do(http.MethodDelete, "/api/v1/resources/"+first.ID, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete resource: status = %d\nbody: %s", w.Code, w.Body.String())
	}
	if w := c.do(http.MethodGet, "/api/v1/resources/"+first.ID, ""); w.Code != http.StatusNotFound {
		t.Errorf("deleted resource still readable: status = %d", w.Code)
	}

	second := c.createResource(`{"name":"Avanza 2022","base_price":360000}`)
	if w := c.do(http.MethodPost, "/api/v1/resources/"+second.ID+"/units",
		`{"code":"B 7777 CC"}`); w.Code != http.StatusCreated {
		t.Fatalf("plate did not move to the new car: status = %d\nbody: %s", w.Code, w.Body.String())
	}
}

// TestUnitCountCountsActiveUnitsOnly is BR-010's visible half: a resource with
// no active units can never appear in availability search, and the catalogue
// screen has to be able to say so rather than showing a healthy-looking row.
func TestUnitCountCountsActiveUnitsOnly(t *testing.T) {
	c := newCatalogClient(t)
	res := c.createResource(`{"name":"Avanza 2021","base_price":350000}`)
	if res.UnitCount != 0 {
		t.Fatalf("fresh resource unit_count = %d, want 0", res.UnitCount)
	}

	w := c.do(http.MethodPost, "/api/v1/resources/"+res.ID+"/units", `{"code":"B 2 BB"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create unit: status = %d\nbody: %s", w.Code, w.Body.String())
	}
	var unit struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &unit); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got := c.getResource(res.ID); got.UnitCount != 1 {
		t.Errorf("unit_count = %d after adding one unit, want 1", got.UnitCount)
	}
	if w := c.do(http.MethodPatch, "/api/v1/units/"+unit.ID,
		`{"status":"maintenance"}`); w.Code != http.StatusOK {
		t.Fatalf("maintenance: status = %d\nbody: %s", w.Code, w.Body.String())
	}
	if got := c.getResource(res.ID); got.UnitCount != 0 {
		t.Errorf("unit_count = %d with its only unit in maintenance, want 0 (BR-010, BR-013)",
			got.UnitCount)
	}
}

func (c catalogClient) getResource(id string) resourceBodyShape {
	c.t.Helper()
	w := c.do(http.MethodGet, "/api/v1/resources/"+id, "")
	if w.Code != http.StatusOK {
		c.t.Fatalf("get resource: status = %d\nbody: %s", w.Code, w.Body.String())
	}
	var out resourceBodyShape
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		c.t.Fatalf("decode: %v", err)
	}
	return out
}

// TestOperatorReadsTheCatalogueButCannotWriteIt is BR-003 at this endpoint.
//
// The operator's whole job -- booking, handover, payment -- points at the
// catalogue, so reading is theirs. Writing is not, and the 403 names the
// permission so the answer is "ask your owner for resources:write" rather than
// a blank refusal.
func TestOperatorReadsTheCatalogueButCannotWriteIt(t *testing.T) {
	ctx := t.Context()
	store := openAppStore(ctx, t)
	srv := newServer(t)

	ownerID := uuid.Must(uuid.NewV7())
	// seedAuthUser leaves role = 'operator', which is exactly what this needs.
	operator := seedSignedInUser(ctx, t, store, ownerID)

	as := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, path, nil)
		} else {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		}
		r.Header.Set("Authorization", "Bearer "+operator.accessToken)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}

	if w := as(http.MethodGet, "/api/v1/resources", ""); w.Code != http.StatusOK {
		t.Errorf("operator GET /resources: status = %d, want 200 -- reading the "+
			"catalogue is implied by everything an operator does\nbody: %s", w.Code, w.Body.String())
	}

	id := uuid.Must(uuid.NewV7()).String()
	for _, tc := range []struct {
		name, method, path, body, permission string
	}{
		{"create resource", http.MethodPost, "/api/v1/resources", `{"name":"X","base_price":1}`, "resources:write"},
		{"update resource", http.MethodPatch, "/api/v1/resources/" + id, `{"name":"X"}`, "resources:write"},
		{"delete resource", http.MethodDelete, "/api/v1/resources/" + id, "", "records:delete"},
		{"create unit", http.MethodPost, "/api/v1/resources/" + id + "/units", `{"code":"X"}`, "units:write"},
		{"update unit", http.MethodPatch, "/api/v1/units/" + id, `{"status":"maintenance"}`, "units:write"},
		{"delete unit", http.MethodDelete, "/api/v1/units/" + id, "", "records:delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := as(tc.method, tc.path, tc.body)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403\nbody: %s", w.Code, w.Body.String())
			}
			code, detail, _ := problemOf(t, w)
			if code != "permission-denied" {
				t.Errorf("code = %q, want permission-denied", code)
			}
			if !strings.Contains(detail, tc.permission) {
				t.Errorf("detail %q does not name %q", detail, tc.permission)
			}
		})
	}
}

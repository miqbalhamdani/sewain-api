package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/owner"
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

// newEquipmentClient is newCatalogClient on the other phase-1 preset.
//
// The preset is changed after seeding rather than threaded through the fixture:
// business_type is read from the row inside the transaction, never from the
// token, so an UPDATE is enough and the seeded session stays valid (BR-017).
func newEquipmentClient(t *testing.T) catalogClient {
	t.Helper()
	ctx := t.Context()
	store := openAppStore(ctx, t)
	ownerID := uuid.Must(uuid.NewV7())
	s := seedSignedInOwner(ctx, t, store, ownerID)

	if err := store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE owners SET business_type = 'equipment_rental' WHERE id = $1`, ownerID)
		return err
	}); err != nil {
		t.Fatalf("switch preset: %v", err)
	}
	return catalogClient{t: t, srv: newServer(t), token: s.accessToken}
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
type vehicleShape struct {
	VehicleType  string `json:"vehicle_type"`
	Transmission string `json:"transmission"`
	Seats        *int   `json:"seats"`
	Fuel         string `json:"fuel"`
}

type resourceBodyShape struct {
	ID             string        `json:"id"`
	Category       *string       `json:"category"`
	Description    *string       `json:"description"`
	Vehicle        *vehicleShape `json:"vehicle"`
	Name           string        `json:"name"`
	PricingUnit    string        `json:"pricing_unit"`
	BasePrice      int64         `json:"base_price"`
	DepositAmount  *int64        `json:"deposit_amount"`
	LateFeePerUnit *int64        `json:"late_fee_per_unit"`
	MinDuration    *int          `json:"min_duration"`
	MaxDuration    *int          `json:"max_duration"`
	BufferMinutes  int           `json:"buffer_minutes"`
	Status         string        `json:"status"`
	UnitCount      int           `json:"unit_count"`
	ActiveBookings int           `json:"active_bookings"`
}

// withCar splices a minimal car spec into a resource body that does not name
// one.
//
// The fixture rental is vehicle_rental, and since S1-085 that preset must send
// `vehicle` or be refused (BR-094). Every test below that is about something
// else -- BR-016 nominals, unit codes, permissions -- would otherwise carry four
// lines of vehicle noise obscuring the one line it is actually asserting.
//
// Tests that ARE about the vehicle rules pass their own, and the ones that check
// the refusal deliberately do not call this.
func withCar(body string) string {
	if strings.Contains(body, `"vehicle"`) {
		return body
	}
	const spec = `,"vehicle":{"vehicle_type":"car","transmission":"manual","seats":7,"fuel":"gasoline"}}`
	return strings.TrimSuffix(body, "}") + spec
}

// createResourceRaw sends the body exactly as given, for the presets that must
// not carry a vehicle at all.
func (c catalogClient) createResourceRaw(body string) resourceBodyShape {
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

func (c catalogClient) createResource(body string) resourceBodyShape {
	c.t.Helper()
	w := c.do(http.MethodPost, "/api/v1/resources", withCar(body))
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
				withCar(`{"name":"Nol","base_price":1000,"`+field+`":0}`))
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
		withCar(`{"name":"Terbalik","base_price":1000,"min_duration":7,"max_duration":3}`))
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
		// BR-094: category is derived from vehicle_type, never sent.
		{"category", `{"name":"Lapangan","base_price":100000,"category":"Mobil"}`, "category"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := c.do(http.MethodPost, "/api/v1/resources", withCar(tc.body))
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

// The acceptance for S1-085 and S1-086: vehicle attributes, terms, profil usaha.
//
// Almost every case below is a CHECK the database owns. They are exercised
// through HTTP anyway, because what is being proven is not that PostgreSQL can
// refuse a row -- it is that the refusal reaches the caller as the 422 the
// contract promised, on the field they have to change. That is the exact hop
// M1 got wrong once already.

const carSpec = `"vehicle":{"vehicle_type":"car","transmission":"manual","seats":7,"fuel":"gasoline"}`

// TestVehicleObligationRunsBothWays is BR-094's one rule that Go owns rather
// than the database.
//
// A database can refuse a child pointing at the wrong parent, and the composite
// key does. What it cannot cheaply do is insist a child EXISTS, or know which
// presets are allowed one. Both directions are tested because refusing only the
// missing half would let an equipment rental send specs that are silently
// thrown away -- data the caller believes is saved.
func TestVehicleObligationRunsBothWays(t *testing.T) {
	c := newCatalogClient(t)

	w := c.do(http.MethodPost, "/api/v1/resources", `{"name":"Tanpa Spek","base_price":1000}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("vehicle_rental tanpa vehicle: status = %d, want 422\nbody: %s", w.Code, w.Body.String())
	}
	_, _, fields := problemOf(t, w)
	if len(fields) == 0 || fields[0] != "vehicle" {
		t.Errorf("errors[].field = %v, want [vehicle]", fields)
	}

	// The other direction needs a rental on a different preset.
	e := newEquipmentClient(t)
	w = e.do(http.MethodPost, "/api/v1/resources",
		`{"name":"Kamera Sony","base_price":250000,`+carSpec+`}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("equipment_rental dengan vehicle: status = %d, want 422\nbody: %s", w.Code, w.Body.String())
	}
	// ...and without it the same rental saves fine, with no vehicle on the way back.
	got := e.createResourceRaw(`{"name":"Kamera Sony","base_price":250000}`)
	if got.Vehicle != nil {
		t.Errorf("equipment resource carries a vehicle: %+v", got.Vehicle)
	}
	if got.Category != nil {
		t.Errorf("category = %v, want null for a preset with no companion table", *got.Category)
	}
}

// TestCrossColumnRulesAreRefusedByTheDatabase walks all three, both sides.
//
// The seats rule is written as an equality on purpose, and the second half is
// the one that would rot silently: a CHECK reading "a car must have seats"
// passes the first case here and accepts a four-seat motorcycle forever.
func TestCrossColumnRulesAreRefusedByTheDatabase(t *testing.T) {
	c := newCatalogClient(t)

	for _, tc := range []struct{ name, vehicle, field string }{
		{"mobil tanpa kursi", `{"vehicle_type":"car","transmission":"manual","fuel":"gasoline"}`, "vehicle.seats"},
		{"motor berkursi", `{"vehicle_type":"motorcycle","transmission":"manual","seats":2,"fuel":"gasoline"}`, "vehicle.seats"},
		{"kopling di mobil", `{"vehicle_type":"car","transmission":"clutch","seats":5,"fuel":"gasoline"}`, "vehicle.transmission"},
		{"diesel di motor", `{"vehicle_type":"motorcycle","transmission":"manual","fuel":"diesel"}`, "vehicle.fuel"},
		{"kursi 21", `{"vehicle_type":"car","transmission":"manual","seats":21,"fuel":"gasoline"}`, "vehicle.seats"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := c.do(http.MethodPost, "/api/v1/resources",
				`{"name":"X","base_price":1000,"vehicle":`+tc.vehicle+`}`)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422\nbody: %s", w.Code, w.Body.String())
			}
			code, _, fields := problemOf(t, w)
			if code != "validation-failed" {
				t.Errorf("code = %q, want validation-failed", code)
			}
			if len(fields) == 0 || fields[0] != tc.field {
				t.Errorf("errors[].field = %v, want [%s]", fields, tc.field)
			}
		})
	}

	// And the legal shapes stay legal, so a constraint that over-reaches shows up.
	for _, ok := range []string{
		`{"vehicle_type":"motorcycle","transmission":"clutch","fuel":"gasoline"}`,
		`{"vehicle_type":"car","transmission":"automatic","seats":2,"fuel":"diesel"}`,
		`{"vehicle_type":"car","transmission":"manual","seats":20,"fuel":"electric"}`,
	} {
		if w := c.do(http.MethodPost, "/api/v1/resources",
			`{"name":"Sah","base_price":1000,"vehicle":`+ok+`}`); w.Code != http.StatusCreated {
			t.Errorf("kombinasi sah ditolak: status = %d\nbody: %s", w.Code, w.Body.String())
		}
	}
}

// TestCategoryIsDerivedNeverSent is BR-094's second rule, and it is the pattern
// pricing_unit already established (BR-017 rule 1).
func TestCategoryIsDerivedNeverSent(t *testing.T) {
	c := newCatalogClient(t)

	got := c.createResource(`{"name":"Avanza 1.3 G","base_price":350000}`)
	if got.Category == nil || *got.Category != "car" {
		t.Errorf("category = %v, want \"car\" derived from vehicle_type", got.Category)
	}

	moto := c.createResource(`{"name":"Vario 160","base_price":90000,` +
		`"vehicle":{"vehicle_type":"motorcycle","transmission":"automatic","fuel":"gasoline"}}`)
	if moto.Category == nil || *moto.Category != "motorcycle" {
		t.Errorf("category = %v, want \"motorcycle\"", moto.Category)
	}
}

// TestVehicleTypeIsLocked: it is not in the update schema at all, so the only
// way to reach it is a hand-rolled body -- which is silently ignored rather than
// applied. The point of the test is that the stored value does not move.
func TestVehicleTypeIsLocked(t *testing.T) {
	c := newCatalogClient(t)
	got := c.createResource(`{"name":"Avanza 1.3 G","base_price":350000}`)

	w := c.do(http.MethodPatch, "/api/v1/resources/"+got.ID,
		`{"vehicle":{"vehicle_type":"motorcycle","transmission":"automatic","fuel":"gasoline"}}`)
	if w.Code != http.StatusUnprocessableEntity {
		// Without seats the car-side CHECK fires, which is itself the proof the
		// type never moved: a real motorcycle would have been accepted.
		t.Fatalf("status = %d, want 422\nbody: %s", w.Code, w.Body.String())
	}

	after := c.getResource(got.ID)
	if after.Vehicle == nil || after.Vehicle.VehicleType != "car" {
		t.Errorf("vehicle_type moved: %+v", after.Vehicle)
	}
}

// TestVehiclePatchReplacesTheWholeObject: `vehicle` is a nested object the form
// renders whole, so what arrives is what the resource should have.
func TestVehiclePatchReplacesTheWholeObject(t *testing.T) {
	c := newCatalogClient(t)
	got := c.createResource(`{"name":"Avanza 1.3 G","base_price":350000}`)

	w := c.do(http.MethodPatch, "/api/v1/resources/"+got.ID,
		`{"vehicle":{"transmission":"automatic","seats":5,"fuel":"hybrid"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d\nbody: %s", w.Code, w.Body.String())
	}
	after := c.getResource(got.ID)
	if after.Vehicle == nil || after.Vehicle.Transmission != "automatic" ||
		after.Vehicle.Fuel != "hybrid" || after.Vehicle.Seats == nil || *after.Vehicle.Seats != 5 {
		t.Errorf("spek tidak terganti: %+v", after.Vehicle)
	}

	// A PATCH that never mentions vehicle leaves it exactly where it was.
	if w := c.do(http.MethodPatch, "/api/v1/resources/"+got.ID,
		`{"name":"Avanza Baru"}`); w.Code != http.StatusOK {
		t.Fatalf("status = %d\nbody: %s", w.Code, w.Body.String())
	}
	if again := c.getResource(got.ID); again.Vehicle == nil || again.Vehicle.Fuel != "hybrid" {
		t.Errorf("vehicle hilang setelah PATCH yang tidak menyebutnya: %+v", again.Vehicle)
	}
}

// TestUnitCarriesItsVehicleDetail covers S1-088's backend half.
func TestUnitCarriesItsVehicleDetail(t *testing.T) {
	c := newCatalogClient(t)
	res := c.createResource(`{"name":"Avanza 1.3 G","base_price":350000}`)
	units := "/api/v1/resources/" + res.ID + "/units"

	w := c.do(http.MethodPost, units,
		`{"code":"B 1234 XY","vehicle":{"year":2021,"color":"Putih","tax_due_on":"2027-03-15"}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d\nbody: %s", w.Code, w.Body.String())
	}
	var unit struct {
		ID      string `json:"id"`
		Vehicle *struct {
			Year     int     `json:"year"`
			Color    *string `json:"color"`
			TaxDueOn *string `json:"tax_due_on"`
		} `json:"vehicle"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &unit); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if unit.Vehicle == nil || unit.Vehicle.Year != 2021 {
		t.Fatalf("detail kendaraan tidak kembali: %+v", unit.Vehicle)
	}
	if unit.Vehicle.TaxDueOn == nil || *unit.Vehicle.TaxDueOn != "2027-03-15" {
		t.Errorf("tax_due_on = %v, want 2027-03-15", unit.Vehicle.TaxDueOn)
	}

	// Year 1989 is refused by the database, not by the handler.
	if w := c.do(http.MethodPost, units,
		`{"code":"B 9 OLD","vehicle":{"year":1989}}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("tahun 1989: status = %d, want 422\nbody: %s", w.Code, w.Body.String())
	}

	// A wholesale replace is how a mistyped date gets cleared again.
	if w := c.do(http.MethodPatch, "/api/v1/units/"+unit.ID,
		`{"vehicle":{"year":2021,"color":"Hitam"}}`); w.Code != http.StatusOK {
		t.Fatalf("patch: status = %d\nbody: %s", w.Code, w.Body.String())
	}
	w = c.do(http.MethodGet, units, "")
	if strings.Contains(w.Body.String(), "2027-03-15") {
		t.Error("tax_due_on masih ada setelah diganti dengan objek tanpa tanggal")
	}
}

// TestTermsLengthIsRefusedByTheDatabase is BR-095: the public page renders these
// verbatim, so 50k characters is not a rental term, it is a broken page.
func TestTermsLengthIsRefusedByTheDatabase(t *testing.T) {
	c := newCatalogClient(t)
	w := c.do(http.MethodPost, "/api/v1/resources",
		`{"name":"Panjang","base_price":1000,`+carSpec+`,"terms_excludes":"`+strings.Repeat("x", 501)+`"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422\nbody: %s", w.Code, w.Body.String())
	}
	_, _, fields := problemOf(t, w)
	if len(fields) == 0 || fields[0] != "terms_excludes" {
		t.Errorf("errors[].field = %v, want [terms_excludes]", fields)
	}

	got := c.createResource(`{"name":"Pas","base_price":1000,"description":"AC dingin, charger HP.",` +
		`"terms_requirements":"KTP & SIM A asli penyewa."}`)
	if got.Description == nil || *got.Description != "AC dingin, charger HP." {
		t.Errorf("description = %v", got.Description)
	}
}

// TestWhatsAppFormatIsRefusedByTheDatabase is BR-096. The public page turns it
// into a wa.me link, and a dead link on the page whose whole purpose is reaching
// the owner is worse than no button at all.
func TestWhatsAppFormatIsRefusedByTheDatabase(t *testing.T) {
	c := newCatalogClient(t)

	w := c.do(http.MethodPatch, "/api/v1/settings", `{"whatsapp":"08123456789"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("nomor lokal: status = %d, want 422\nbody: %s", w.Code, w.Body.String())
	}
	_, detail, fields := problemOf(t, w)
	if len(fields) == 0 || fields[0] != "whatsapp" {
		t.Errorf("errors[].field = %v, want [whatsapp]", fields)
	}
	if !strings.Contains(detail, "+62") {
		t.Errorf("detail %q does not show the shape it wants", detail)
	}

	w = c.do(http.MethodPatch, "/api/v1/settings",
		`{"whatsapp":"+628123456789","address":"Jl. Kaliurang KM 5","operating_hours":"Senin-Sabtu 08.00-20.00"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d\nbody: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "+628123456789") {
		t.Errorf("profil tidak kembali di respons: %s", w.Body.String())
	}
}

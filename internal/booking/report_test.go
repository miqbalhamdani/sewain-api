package booking

import (
	"bytes"
	"encoding/csv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"

	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
)

// The absorbed deposit lands on the charges in settle's order, and stops at
// what was deducted -- the rest went to a new invoice, counted when paid.
func TestAllocateAbsorbed(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	got := allocateAbsorbed([]sqlcgen.AbsorbedChargesRow{
		{BookingID: a, DepositDeducted: 250000, Kind: "late_fee", Amount: 200000},
		{BookingID: a, DepositDeducted: 250000, Kind: "damage", Amount: 100000},
		{BookingID: b, DepositDeducted: 80000, Kind: "damage", Amount: 80000},
	})
	if got["late_fee"] != 200000 || got["damage"] != 130000 {
		t.Errorf("allocation = %v, want late_fee 200000, damage 50000+80000", got)
	}
}

// S1-056 against real PostgreSQL: cash-basis revenue with an absorbed
// deposit, deposit held apart, utilisation clipped to the period, idle units
// including one never rented; then the same numbers through both export
// formats (S1-057).
func TestReports(t *testing.T) {
	f := newFixture(t, 3)
	s := New(f.store, nil)
	now := time.Now()
	from, to := now.AddDate(0, 0, -30), now

	booking := func(code, status string, unit int, start, end time.Time, deposit *int64) uuid.UUID {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		f.exec(t, `INSERT INTO bookings (id, owner_id, code, customer_id, resource_id, resource_unit_id,
		             start_at, end_at, status, unit_price, pricing_unit, duration_qty, subtotal, deposit_amount)
		           VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1, 'day', 1, 1, $10)`,
			id, f.ownerID, code, f.customer, f.resource, f.units[unit], start, end, status, deposit)
		return id
	}
	invoice := func(b uuid.UUID, status string, paidAt *time.Time, lines map[string]int64) {
		t.Helper()
		id := uuid.Must(uuid.NewV7())
		f.exec(t, `INSERT INTO invoices (id, owner_id, booking_id, number, due_at, status, paid_at)
		           VALUES ($1, $2, $3, $4, now(), $5, $6)`, id, f.ownerID, b, "INV-"+id.String(), status, paidAt)
		for kind, amount := range lines {
			f.exec(t, `INSERT INTO invoice_lines (id, owner_id, invoice_id, kind, description, amount)
			           VALUES ($1, $2, $3, $4, $4, $5)`, uuid.Must(uuid.NewV7()), f.ownerID, id, kind, amount)
		}
	}
	at := func(d time.Time) *time.Time { return &d }
	deposit := func(n int64) *int64 { return &n }

	// Unit 0: 10 of the period's 30 days out, starting 5 days before the
	// period -- only the 10 inside count. Rent, discount and deposit paid in
	// the period; the late fee absorbed 250,000 of the deposit at settlement.
	a := booking("A", "completed", 0, from.AddDate(0, 0, -5), from.AddDate(0, 0, 10), deposit(500000))
	invoice(a, "paid", at(from.AddDate(0, 0, 1)), map[string]int64{"rent": 300000, "discount": -50000, "deposit": 500000})
	invoice(a, "cancelled", nil, map[string]int64{"late_fee": 250000})
	f.exec(t, `UPDATE bookings SET deposit_settled_at = $2, deposit_deducted = 250000, deposit_refunded = 250000
	           WHERE id = $1`, a, from.AddDate(0, 0, 12))
	// Paid before the period: not this period's money.
	invoice(a, "paid", at(from.AddDate(0, 0, -1)), map[string]int64{"rent": 999})

	// Unit 1: last out 40 days ago -> idle. Its deposit is paid and held.
	b := booking("B", "returned", 1, now.AddDate(0, 0, -45), now.AddDate(0, 0, -40), deposit(100000))
	invoice(b, "paid", at(from.AddDate(0, 0, 2)), map[string]int64{"deposit": 100000})

	// Unit 2: never rented, created 60 days ago -> idle since then.
	f.exec(t, `UPDATE resource_units SET created_at = $2 WHERE id = $1`, f.units[2], now.AddDate(0, 0, -60))

	// Another rental's money in the same period never reaches these sums.
	other := newFixture(t, 1)
	ob := uuid.Must(uuid.NewV7())
	other.exec(t, `INSERT INTO bookings (id, owner_id, code, customer_id, resource_id, resource_unit_id,
	                 start_at, end_at, status, unit_price, pricing_unit, duration_qty, subtotal)
	               VALUES ($1, $2, 'O', $3, $4, $5, $6, $7, 'completed', 1, 'day', 1, 1)`,
		ob, other.ownerID, other.customer, other.resource, other.units[0], from, to)
	oi := uuid.Must(uuid.NewV7())
	other.exec(t, `INSERT INTO invoices (id, owner_id, booking_id, number, due_at, status, paid_at)
	               VALUES ($1, $2, $3, 'O-1', now(), 'paid', $4)`, oi, other.ownerID, ob, from.AddDate(0, 0, 3))
	other.exec(t, `INSERT INTO invoice_lines (id, owner_id, invoice_id, kind, description, amount)
	               VALUES ($1, $2, $3, 'rent', 'Sewa', 777)`, uuid.Must(uuid.NewV7()), other.ownerID, oi)

	rev, err := s.Revenue(f.ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	want := Revenue{Rent: 300000, LateFee: 250000, Discount: -50000, Total: 500000,
		DepositIn: 600000, DepositReturned: 250000, DepositBalance: 100000}
	if rev != want {
		t.Errorf("revenue =\n %+v\nwant\n %+v", rev, want)
	}

	use, err := s.Utilization(f.ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range use {
		wantDays := 0.0
		if u.Unit.ID == f.units[0] {
			wantDays = 10
		}
		if d := u.RentedDays - wantDays; d > 0.01 || d < -0.01 {
			t.Errorf("unit %s rented %.2f days, want %.0f", u.Unit.Code, u.RentedDays, wantDays)
		}
	}

	idle, err := s.IdleUnits(f.ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(idle) != 2 || idle[0].Unit.ID != f.units[2] || idle[0].LastRentedAt != nil || idle[0].IdleDays != 60 ||
		idle[1].Unit.ID != f.units[1] || idle[1].LastRentedAt == nil || idle[1].IdleDays != 40 {
		t.Errorf("idle units = %+v, want unit 3 (never, 60 days) then unit 2 (40 days)", idle)
	}

	// Export: deposit stays its own columns (BR-050), in both formats.
	body, _, ext, err := s.Export(f.ctx, "revenue", "csv", from, to, now)
	if err != nil || ext != "csv" {
		t.Fatalf("csv export: %v", err)
	}
	recs, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil || len(recs) != 2 || recs[0][6] != "total_pemasukan" || recs[1][6] != "500000" || recs[1][9] != "100000" {
		t.Errorf("csv = %v (%v)", recs, err)
	}
	body, _, ext, err = s.Export(f.ctx, "idle_units", "xlsx", from, to, now)
	if err != nil || ext != "xlsx" {
		t.Fatalf("xlsx export: %v", err)
	}
	x, err := excelize.OpenReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := x.GetRows(x.GetSheetName(0))
	if len(rows) != 3 || rows[1][3] != "belum pernah" {
		t.Errorf("xlsx rows = %v", rows)
	}
}

// S1-063: the four lists and the onboarding checklist, from data.
func TestDashboard(t *testing.T) {
	f := newFixture(t, 3)
	s := New(f.store, nil)
	now := time.Now()
	insert := func(code, status string, unit int, start, end time.Time, deposit *int64) {
		t.Helper()
		f.exec(t, `INSERT INTO bookings (id, owner_id, code, customer_id, resource_id, resource_unit_id,
		             start_at, end_at, status, unit_price, pricing_unit, duration_qty, subtotal, deposit_amount)
		           VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, 1, 'day', 1, 1, $9)`,
			f.ownerID, code, f.customer, f.resource, f.units[unit], start, end, status, deposit)
	}

	d, err := s.Dashboard(f.ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if !d.HasResource || !d.HasUnit || d.HasBooking {
		t.Errorf("onboarding before any booking = %+v", d)
	}

	dep := int64(100000)
	insert("LATE", "picked_up", 0, now.Add(-48*time.Hour), now.Add(-time.Hour), nil)
	insert("BACK", "returned", 1, now.Add(-96*time.Hour), now.Add(-72*time.Hour), &dep)
	insert("TODAY", "reserved", 2, now, now.Add(24*time.Hour), nil)

	if d, err = s.Dashboard(f.ctx, now); err != nil {
		t.Fatal(err)
	}
	code := func(bs []BookingBrief) string {
		if len(bs) != 1 {
			return "?"
		}
		return bs[0].Code
	}
	if code(d.Overdue) != "LATE" || code(d.UnsettledDeposits) != "BACK" || code(d.TodayPickups) != "TODAY" ||
		len(d.TodayReturns) != 0 || !d.HasBooking {
		t.Errorf("dashboard = %+v", d)
	}
}

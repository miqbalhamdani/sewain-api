package booking

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
)

// Reports and the dashboard.  (S1-056, S1-063, BR-075 .. BR-077)
//
// Revenue is cash basis: money counts when it arrives -- an invoice's paid_at,
// or deposit_settled_at for charges a deposit absorbed. Deposit is never
// revenue; it is reported beside it as funds held (BR-050, BR-076).

type Revenue struct {
	Rent, LateFee, Damage, Discount, Total     int64
	DepositIn, DepositReturned, DepositBalance int64
}

func (s *Service) Revenue(ctx context.Context, from, to time.Time) (Revenue, error) {
	if err := checkRange(from, to, "to"); err != nil {
		return Revenue{}, err
	}
	var r Revenue
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		lines, err := q.RevenuePaidLines(ctx, sqlcgen.RevenuePaidLinesParams{FromAt: from, ToAt: to})
		if err != nil {
			return err
		}
		for _, l := range lines {
			r.add(l.Kind, l.Amount)
		}
		absorbed, err := q.AbsorbedCharges(ctx, sqlcgen.AbsorbedChargesParams{FromAt: from, ToAt: to})
		if err != nil {
			return err
		}
		for kind, amount := range allocateAbsorbed(absorbed) {
			r.add(kind, amount)
		}
		if r.DepositReturned, err = q.DepositsReturned(ctx, sqlcgen.DepositsReturnedParams{FromAt: from, ToAt: to}); err != nil {
			return err
		}
		r.DepositBalance, err = q.DepositBalance(ctx)
		return err
	})
	if err != nil {
		return Revenue{}, fmt.Errorf("revenue report: %w", err)
	}
	r.Total = r.Rent + r.LateFee + r.Damage + r.Discount
	return r, nil
}

func (r *Revenue) add(kind string, amount int64) {
	switch kind {
	case "rent":
		r.Rent += amount
	case "late_fee":
		r.LateFee += amount
	case "damage":
		r.Damage += amount
	case "discount":
		r.Discount += amount
	case "deposit":
		r.DepositIn += amount // held, not revenue (BR-050)
	}
}

// allocateAbsorbed splits each settled booking's deducted deposit over the
// charges it absorbed, in the order settle consumed them -- the same walk as
// absorb() -- so the absorbed money lands under late_fee and damage.
func allocateAbsorbed(rows []sqlcgen.AbsorbedChargesRow) map[string]int64 {
	out := map[string]int64{}
	left := map[uuid.UUID]int64{}
	seen := map[uuid.UUID]bool{}
	for _, r := range rows {
		if !seen[r.BookingID] {
			seen[r.BookingID], left[r.BookingID] = true, r.DepositDeducted
		}
		take := min(left[r.BookingID], r.Amount)
		left[r.BookingID] -= take
		out[r.Kind] += take
	}
	return out
}

type UnitUsage struct {
	Unit                   UnitRef
	ResourceName           string
	RentedDays, PeriodDays float64
	Utilization            float64 // 0..1
}

func (s *Service) Utilization(ctx context.Context, from, to time.Time) ([]UnitUsage, error) {
	if err := checkRange(from, to, "to"); err != nil {
		return nil, err
	}
	var rows []sqlcgen.UnitUtilizationRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlcgen.New(tx).UnitUtilization(ctx, sqlcgen.UnitUtilizationParams{FromAt: from, ToAt: to})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("utilization report: %w", err)
	}
	period := to.Sub(from).Hours() / 24
	out := make([]UnitUsage, 0, len(rows))
	for _, r := range rows {
		days := min(r.RentedSeconds/86400, period) // a late return cannot rent more days than the period has
		out = append(out, UnitUsage{Unit: UnitRef{ID: r.ID, Code: r.Code, Label: r.Label},
			ResourceName: r.ResourceName, RentedDays: days, PeriodDays: period, Utilization: days / period})
	}
	return out, nil
}

// IdleAfter is BR-075's "menganggur > 30 hari".
const IdleAfter = 30 * 24 * time.Hour

type IdleUnit struct {
	Unit         UnitRef
	ResourceName string
	LastRentedAt *time.Time
	IdleDays     int
}

func (s *Service) IdleUnits(ctx context.Context, now time.Time) ([]IdleUnit, error) {
	var rows []sqlcgen.IdleUnitsRow
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = sqlcgen.New(tx).IdleUnits(ctx, now.Add(-IdleAfter))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("idle units report: %w", err)
	}
	out := make([]IdleUnit, 0, len(rows))
	for _, r := range rows {
		u := IdleUnit{Unit: UnitRef{ID: r.ID, Code: r.Code, Label: r.Label}, ResourceName: r.ResourceName,
			IdleDays: int(now.Sub(r.IdleSince).Hours() / 24)}
		if r.EverRented {
			at := r.IdleSince
			u.LastRentedAt = &at
		}
		out = append(out, u)
	}
	return out, nil
}

// Dashboard is what needs handling today (S1-063). Revenue is filled by the
// caller only for an owner (BR-003).
type Dashboard struct {
	Overdue, UnsettledDeposits, TodayPickups, TodayReturns []BookingBrief
	UnpaidCount, OverdueCount                              int
	Outstanding                                            int64
	HasResource, HasUnit, HasBooking                       bool
}

type BookingBrief struct {
	ID                         uuid.UUID
	Code, Status               string
	CustomerName, ResourceName string
	Unit                       UnitRef
	StartAt, EndAt             time.Time
}

var jakarta = time.FixedZone("WIB", 7*3600)

func (s *Service) Dashboard(ctx context.Context, now time.Time) (Dashboard, error) {
	local := now.In(jakarta)
	dayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, jakarta)
	dayEnd := dayStart.AddDate(0, 0, 1)
	var d Dashboard
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		list := func(p sqlcgen.DashboardBookingsParams) ([]BookingBrief, error) {
			rows, err := q.DashboardBookings(ctx, p)
			if err != nil {
				return nil, err
			}
			out := make([]BookingBrief, 0, len(rows))
			for _, r := range rows {
				out = append(out, BookingBrief{ID: r.ID, Code: r.Code, Status: r.Status, CustomerName: r.CustomerName,
					ResourceName: r.ResourceName, Unit: UnitRef{ID: r.UnitID, Code: r.UnitCode, Label: r.UnitLabel},
					StartAt: r.StartAt, EndAt: r.EndAt})
			}
			return out, nil
		}
		var err error
		if d.Overdue, err = list(sqlcgen.DashboardBookingsParams{Status: "picked_up", EndTo: &now}); err != nil {
			return err
		}
		if d.UnsettledDeposits, err = list(sqlcgen.DashboardBookingsParams{Status: "returned", UnsettledDeposit: true}); err != nil {
			return err
		}
		if d.TodayPickups, err = list(sqlcgen.DashboardBookingsParams{Status: "reserved", StartFrom: &dayStart, StartTo: &dayEnd}); err != nil {
			return err
		}
		if d.TodayReturns, err = list(sqlcgen.DashboardBookingsParams{Status: "picked_up", EndFrom: &now, EndTo: &dayEnd}); err != nil {
			return err
		}
		inv, err := q.InvoiceSummary(ctx)
		if err != nil {
			return err
		}
		d.UnpaidCount, d.OverdueCount, d.Outstanding = int(inv.UnpaidCount), int(inv.OverdueCount), inv.Outstanding
		ob, err := q.OnboardingState(ctx)
		if err != nil {
			return err
		}
		d.HasResource, d.HasUnit, d.HasBooking = ob.HasResource, ob.HasUnit, ob.HasBooking
		return nil
	})
	if err != nil {
		return Dashboard{}, fmt.Errorf("dashboard: %w", err)
	}
	return d, nil
}

// MonthStart is the first instant of now's month in WIB.
func MonthStart(now time.Time) time.Time {
	l := now.In(jakarta)
	return time.Date(l.Year(), l.Month(), 1, 0, 0, 0, 0, jakarta)
}

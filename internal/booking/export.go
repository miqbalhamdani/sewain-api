package booking

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"strconv"
	"time"

	"github.com/xuri/excelize/v2"

	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// Report export.  (S1-057, BR-077)
//
// Built by the worker, never in a request (BR-091). Deposit stays its own
// columns in every format -- in a spreadsheet is exactly where someone sums
// the wrong column (BR-050).

// JobReportExport is the tracked worker job that builds one export.
const JobReportExport = "report.export"

// ExportReports is the closed list of what can be exported.
var ExportReports = map[string]bool{"revenue": true, "utilization": true, "idle_units": true, "bookings": true}

type table struct {
	header []string
	rows   [][]any
}

// Export renders one report as CSV or XLSX: the bytes, their content type,
// and the file extension.
func (s *Service) Export(ctx context.Context, report, format string, from, to, now time.Time) ([]byte, string, string, error) {
	t, err := s.exportTable(ctx, report, from, to, now)
	if err != nil {
		return nil, "", "", err
	}
	switch format {
	case "csv":
		var buf bytes.Buffer
		w := csv.NewWriter(&buf)
		_ = w.Write(t.header)
		for _, r := range t.rows {
			rec := make([]string, len(r))
			for i, v := range r {
				rec[i] = cell(v)
			}
			_ = w.Write(rec)
		}
		w.Flush()
		return buf.Bytes(), "text/csv", "csv", w.Error()
	case "xlsx":
		f := excelize.NewFile()
		defer func() { _ = f.Close() }()
		sheet := f.GetSheetName(0)
		rows := append([][]any{toAny(t.header)}, t.rows...)
		for i, r := range rows {
			addr, _ := excelize.CoordinatesToCellName(1, i+1)
			if err := f.SetSheetRow(sheet, addr, &r); err != nil {
				return nil, "", "", err
			}
		}
		var buf bytes.Buffer
		if err := f.Write(&buf); err != nil {
			return nil, "", "", err
		}
		return buf.Bytes(), "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "xlsx", nil
	}
	return nil, "", "", fmt.Errorf("unknown export format %q", format)
}

func (s *Service) exportTable(ctx context.Context, report string, from, to, now time.Time) (table, error) {
	switch report {
	case "revenue":
		r, err := s.Revenue(ctx, from, to)
		if err != nil {
			return table{}, err
		}
		return table{
			header: []string{"dari", "sampai", "sewa", "denda_telat", "kerusakan", "diskon", "total_pemasukan",
				"deposit_masuk", "deposit_dikembalikan", "saldo_deposit"},
			rows: [][]any{{day(from), day(to), r.Rent, r.LateFee, r.Damage, r.Discount, r.Total,
				r.DepositIn, r.DepositReturned, r.DepositBalance}},
		}, nil
	case "utilization":
		us, err := s.Utilization(ctx, from, to)
		if err != nil {
			return table{}, err
		}
		t := table{header: []string{"kode_unit", "nama_unit", "barang", "hari_disewa", "hari_periode", "pemakaian_persen"}}
		for _, u := range us {
			t.rows = append(t.rows, []any{u.Unit.Code, deref(u.Unit.Label), u.ResourceName,
				round1(u.RentedDays), round1(u.PeriodDays), round1(u.Utilization * 100)})
		}
		return t, nil
	case "idle_units":
		us, err := s.IdleUnits(ctx, now)
		if err != nil {
			return table{}, err
		}
		t := table{header: []string{"kode_unit", "nama_unit", "barang", "terakhir_disewa", "hari_menganggur"}}
		for _, u := range us {
			last := "belum pernah"
			if u.LastRentedAt != nil {
				last = day(*u.LastRentedAt)
			}
			t.rows = append(t.rows, []any{u.Unit.Code, deref(u.Unit.Label), u.ResourceName, last, u.IdleDays})
		}
		return t, nil
	case "bookings":
		// BR-075's fourth report: bookings out past end_at, overlapping the period.
		t := table{header: []string{"kode", "penyewa", "telepon", "barang", "unit", "mulai", "seharusnya_kembali", "terlambat_hari"}}
		var after *Cursor
		for {
			page, next, err := s.List(ctx, Filter{Overdue: true, From: &from, To: &to}, after, 200)
			if err != nil {
				return table{}, err
			}
			for _, b := range page {
				t.rows = append(t.rows, []any{b.Code, b.CustomerName, b.CustomerPhone, b.ResourceName, b.UnitCode,
					day(b.StartAt), day(b.EndAt), int(now.Sub(b.EndAt).Hours() / 24)})
			}
			if next == nil {
				return t, nil
			}
			after = next
		}
	}
	return table{}, apperrors.ValidationFailed("report is revenue, utilization, idle_units or bookings.").
		WithFields(apperrors.Field{Name: "report"})
}

func day(t time.Time) string { return t.In(jakarta).Format("2006-01-02") }

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func cell(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	"github.com/miqbalhamdani/sewain-api/internal/booking"
	"github.com/miqbalhamdani/sewain-api/internal/jobs"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
	"github.com/miqbalhamdani/sewain-api/internal/storage"
)

// Dashboard, reports, export and job status over HTTP.
// (S1-056, S1-057, S1-063, 04-api-spec.md 3.9)

// GetRevenueReport handles GET /reports/revenue.
func (s *Server) GetRevenueReport(w http.ResponseWriter, r *http.Request, params GetRevenueReportParams) {
	requirePermission(auth.PermReportsRead, func(w http.ResponseWriter, r *http.Request) {
		rev, err := s.bookings.Revenue(r.Context(), params.From, params.To)
		if err != nil {
			writeError(w, r, err)
			return
		}
		body := RevenueReport{From: params.From, To: params.To}
		body.Revenue.Rent, body.Revenue.LateFee, body.Revenue.Damage = rev.Rent, rev.LateFee, rev.Damage
		body.Revenue.Discount, body.Revenue.Total = rev.Discount, rev.Total
		body.DepositHeld.In, body.DepositHeld.Returned, body.DepositHeld.Balance = rev.DepositIn, rev.DepositReturned, rev.DepositBalance
		writeJSON(w, r, http.StatusOK, body)
	})(w, r)
}

// GetUtilizationReport handles GET /reports/utilization.
func (s *Server) GetUtilizationReport(w http.ResponseWriter, r *http.Request, params GetUtilizationReportParams) {
	requirePermission(auth.PermReportsRead, func(w http.ResponseWriter, r *http.Request) {
		rows, err := s.bookings.Utilization(r.Context(), params.From, params.To)
		if err != nil {
			writeError(w, r, err)
			return
		}
		body := UtilizationReport{From: params.From, To: params.To}
		body.Data = make([]struct {
			PeriodDays   float32 `json:"period_days"`
			RentedDays   float32 `json:"rented_days"`
			ResourceName string  `json:"resource_name"`
			Unit         UnitRef `json:"unit"`
			Utilization  float32 `json:"utilization"`
		}, len(rows))
		for i, u := range rows {
			d := &body.Data[i]
			d.Unit, d.ResourceName = UnitRef{Id: u.Unit.ID, Code: u.Unit.Code, Label: u.Unit.Label}, u.ResourceName
			d.RentedDays, d.PeriodDays, d.Utilization = float32(u.RentedDays), float32(u.PeriodDays), float32(u.Utilization)
		}
		writeJSON(w, r, http.StatusOK, body)
	})(w, r)
}

// GetIdleUnitsReport handles GET /reports/idle-units.
func (s *Server) GetIdleUnitsReport(w http.ResponseWriter, r *http.Request) {
	requirePermission(auth.PermReportsRead, func(w http.ResponseWriter, r *http.Request) {
		rows, err := s.bookings.IdleUnits(r.Context(), time.Now())
		if err != nil {
			writeError(w, r, err)
			return
		}
		var body IdleUnitsReport
		body.Data = make([]struct {
			IdleDays     int        `json:"idle_days"`
			LastRentedAt *time.Time `json:"last_rented_at"`
			ResourceName string     `json:"resource_name"`
			Unit         UnitRef    `json:"unit"`
		}, len(rows))
		for i, u := range rows {
			d := &body.Data[i]
			d.Unit, d.ResourceName = UnitRef{Id: u.Unit.ID, Code: u.Unit.Code, Label: u.Unit.Label}, u.ResourceName
			d.LastRentedAt, d.IdleDays = u.LastRentedAt, u.IdleDays
		}
		writeJSON(w, r, http.StatusOK, body)
	})(w, r)
}

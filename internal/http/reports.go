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

// ExportReport handles POST /reports/export: queue it and answer at once
// (BR-077, BR-091). The worker builds the file.
func (s *Server) ExportReport(w http.ResponseWriter, r *http.Request) {
	requirePermission(auth.PermReportsRead, func(w http.ResponseWriter, r *http.Request) {
		raw, _, ok := readBody(w, r)
		if !ok {
			return
		}
		var body ExportRequest
		if err := json.Unmarshal(raw, &body); err != nil {
			writeError(w, r, malformed(err))
			return
		}
		if !booking.ExportReports[string(body.Report)] {
			writeError(w, r, apperrors.ValidationFailed("report is revenue, utilization, idle_units or bookings.").
				WithFields(apperrors.Field{Name: "report"}))
			return
		}
		if body.Format != Csv && body.Format != Xlsx {
			writeError(w, r, apperrors.ValidationFailed("format is csv or xlsx.").
				WithFields(apperrors.Field{Name: "format"}))
			return
		}
		if !body.To.After(body.From) || body.To.Sub(body.From) > 366*24*time.Hour {
			writeError(w, r, apperrors.ValidationFailed("to must be after from, and a range is at most 366 days.").
				WithFields(apperrors.Field{Name: "to"}))
			return
		}
		id, _ := owner.FromContext(r.Context())
		jobID, err := s.jobs.Track(r.Context(), booking.JobReportExport, id, map[string]any{
			"report": body.Report, "format": body.Format, "from": body.From, "to": body.To,
		})
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusAccepted, map[string]string{"job_id": jobID})
	})(w, r)
}

// GetJob handles GET /jobs/{id}. The download link is signed afresh on every
// read, for 15 minutes (BR-077) -- asking again is how a link is renewed.
func (s *Server) GetJob(w http.ResponseWriter, r *http.Request, jobID string) {
	requirePermission(auth.PermReportsRead, func(w http.ResponseWriter, r *http.Request) {
		id, _ := owner.FromContext(r.Context())
		st, err := s.jobs.Status(r.Context(), jobID, id)
		if errors.Is(err, jobs.ErrNoJob) {
			writeError(w, r, apperrors.NotFound("No such job in this business."))
			return
		}
		if err != nil {
			writeError(w, r, err)
			return
		}
		body := Job{Id: st.ID, Status: JobStatus(st.State)}
		if st.Error != "" {
			body.Error = &st.Error
		}
		if st.State == "done" && st.FileKey != "" {
			url, err := s.objects.PresignGet(r.Context(), st.FileKey, storage.ExportTTL)
			if err != nil {
				writeError(w, r, err)
				return
			}
			exp := time.Now().Add(storage.ExportTTL)
			body.DownloadUrl, body.ExpiresAt = &url, &exp
		}
		writeJSON(w, r, http.StatusOK, body)
	})(w, r)
}

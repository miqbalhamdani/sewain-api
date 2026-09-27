package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// Owner knobs over HTTP.  (S1-009)
//
// PATCH /settings is the first owner-only endpoint in the system, which makes
// it the first caller of requirePermission. An operator reaching it gets a 403
// naming settings:write rather than a blank refusal (BR-003).

// GetSettings handles GET /settings.
func (s *Server) GetSettings(w http.ResponseWriter, r *http.Request) {
	requirePermission(auth.PermSettingsRead, s.getSettings)(w, r)
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	row, err := s.settings.Get(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, settingsBody(sqlcgen.UpdateSettingsRow(row)))
}

// UpdateSettings handles PATCH /settings.
func (s *Server) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	requirePermission(auth.PermSettingsWrite, s.updateSettings)(w, r)
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var body SettingsUpdate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, r, apperrors.ValidationFailed(
			"The request body is malformed or a field is not in the expected format.").WithCause(err))
		return
	}

	// Every field is a pointer, so nil means the key was absent and the stored
	// value stands. The query COALESCEs on exactly that, which is what keeps
	// PATCH from behaving like a PUT that blanks whatever went unmentioned.
	row, err := s.settings.Update(r.Context(), sqlcgen.UpdateSettingsParams{
		Slug:                       body.Slug,
		BookingCodePrefix:          body.BookingCodePrefix,
		RequirePaymentBeforePickup: body.RequirePaymentBeforePickup,
		DraftExpiryHours:           int32Ptr(body.DraftExpiryHours),
		PaymentDueHours:            int32Ptr(body.PaymentDueHours),
		NoShowToleranceHours:       int32Ptr(body.NoShowToleranceHours),
		NotifyPickupReminder:       body.NotifyPickupReminder,
		NotifyReturnReminder:       body.NotifyReturnReminder,
		NotifyOverdueReminder:      body.NotifyOverdueReminder,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, settingsBody(row))
}

func settingsBody(row sqlcgen.UpdateSettingsRow) Settings {
	return Settings{
		Slug:                       row.Slug,
		BookingCodePrefix:          row.BookingCodePrefix,
		RequirePaymentBeforePickup: row.RequirePaymentBeforePickup,
		DraftExpiryHours:           int(row.DraftExpiryHours),
		PaymentDueHours:            int(row.PaymentDueHours),
		NoShowToleranceHours:       int(row.NoShowToleranceHours),
		NotifyPickupReminder:       row.NotifyPickupReminder,
		NotifyReturnReminder:       row.NotifyReturnReminder,
		NotifyOverdueReminder:      row.NotifyOverdueReminder,
	}
}

// int32Ptr bridges the two generators: the contract says integer so
// oapi-codegen emits int, the column is int so sqlc emits int32. One place to
// convert beats one per field.
func int32Ptr(v *int) *int32 {
	if v == nil {
		return nil
	}
	n := int32(*v) //nolint:gosec // bounded by the schema's minimum and the column's CHECK
	return &n
}

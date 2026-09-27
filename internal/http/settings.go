package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
	"github.com/miqbalhamdani/sewain-api/internal/settings"
)

// Owner knobs over HTTP.  (S1-009)
//
// PATCH /settings is the first owner-only endpoint in the system, which makes
// it the first caller of requirePermission. An operator reaching it gets a 403
// naming settings:write rather than a blank refusal (BR-003).
//
// Nothing here mentions sqlc. The generated row types are regenerated from the
// migrations, so a column rename that reached this file would be a schema
// change rippling into the HTTP layer. internal/settings converts, once.

// GetSettings handles GET /settings.
func (s *Server) GetSettings(w http.ResponseWriter, r *http.Request) {
	requirePermission(auth.PermSettingsRead, s.getSettings)(w, r)
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	knobs, err := s.settings.Get(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, settingsBody(knobs))
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

	// Every field is a pointer, and nil carries straight through as "the key
	// was absent". The query COALESCEs on that, which is what keeps PATCH from
	// behaving like a PUT that blanks whatever went unmentioned.
	knobs, err := s.settings.Update(r.Context(), settings.Patch{
		Slug:                       body.Slug,
		BookingCodePrefix:          body.BookingCodePrefix,
		RequirePaymentBeforePickup: body.RequirePaymentBeforePickup,
		DraftExpiryHours:           body.DraftExpiryHours,
		PaymentDueHours:            body.PaymentDueHours,
		NoShowToleranceHours:       body.NoShowToleranceHours,
		NotifyPickupReminder:       body.NotifyPickupReminder,
		NotifyReturnReminder:       body.NotifyReturnReminder,
		NotifyOverdueReminder:      body.NotifyOverdueReminder,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, settingsBody(knobs))
}

// settingsBody is the domain type becoming the contract type. The two happen
// to have the same shape today; they are not the same thing, and the contract
// is free to rename a field without the database following.
func settingsBody(k settings.Knobs) Settings {
	return Settings{
		Slug:                       k.Slug,
		BookingCodePrefix:          k.BookingCodePrefix,
		RequirePaymentBeforePickup: k.RequirePaymentBeforePickup,
		DraftExpiryHours:           k.DraftExpiryHours,
		PaymentDueHours:            k.PaymentDueHours,
		NoShowToleranceHours:       k.NoShowToleranceHours,
		NotifyPickupReminder:       k.NotifyPickupReminder,
		NotifyReturnReminder:       k.NotifyReturnReminder,
		NotifyOverdueReminder:      k.NotifyOverdueReminder,
	}
}

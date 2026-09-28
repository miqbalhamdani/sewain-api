package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	"github.com/miqbalhamdani/sewain-api/internal/catalog"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// Physical units over HTTP.  (S1-015, S1-017)
//
// The path shape is asymmetric on purpose, straight from 04-api-spec.md section
// 3.2: units are listed and created under their resource, but changed and
// deleted at the top level. A unit's id is enough to find it; its kind is only
// needed to say which list it belongs to. There is no GET /units/{id}.

// ListUnits handles GET /resources/{id}/units.
func (s *Server) ListUnits(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermUnitsRead, func(w http.ResponseWriter, r *http.Request) {
		rows, err := s.catalog.ListUnits(r.Context(), id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		body := make([]ResourceUnit, 0, len(rows))
		for _, unit := range rows {
			body = append(body, unitBody(unit))
		}
		writeJSON(w, r, http.StatusOK, body)
	})(w, r)
}

// CreateUnit handles POST /resources/{id}/units.
func (s *Server) CreateUnit(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermUnitsWrite, func(w http.ResponseWriter, r *http.Request) {
		s.createUnit(w, r, id)
	})(w, r)
}

func (s *Server) createUnit(w http.ResponseWriter, r *http.Request, resourceID uuid.UUID) {
	var body UnitCreate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, r, apperrors.ValidationFailed(
			"The request body is malformed or a field is not in the expected format.").WithCause(err))
		return
	}
	if body.Code == "" {
		// Field-level, because the person has to change this exact input --
		// and it is the one field they typed off a number plate.
		writeError(w, r, apperrors.ValidationFailed("code is required.").
			WithFields(apperrors.Field{Name: "code"}))
		return
	}

	userID, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(w, r, apperrors.Unauthenticated("A bearer token is required."))
		return
	}

	unit, err := s.catalog.CreateUnit(r.Context(), userID, resourceID, catalog.NewUnit{
		Code:           body.Code,
		Label:          body.Label,
		MeterValue:     body.MeterValue,
		ConditionNotes: body.ConditionNotes,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, unitBody(unit))
}

// UpdateUnit handles PATCH /units/{id}.
func (s *Server) UpdateUnit(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermUnitsWrite, func(w http.ResponseWriter, r *http.Request) {
		s.updateUnit(w, r, id)
	})(w, r)
}

func (s *Server) updateUnit(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	var body UnitUpdate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, r, apperrors.ValidationFailed(
			"The request body is malformed or a field is not in the expected format.").WithCause(err))
		return
	}

	// No raw-key reading here, unlike PATCH /resources/{id}: none of these
	// fields is a BR-016 nominal, so absent can mean "leave it" throughout and
	// there is nothing a null would have to revoke.
	unit, err := s.catalog.UpdateUnit(r.Context(), id, catalog.UnitPatch{
		Code:           body.Code,
		Label:          body.Label,
		Status:         stringPtr(body.Status),
		MeterValue:     body.MeterValue,
		ConditionNotes: body.ConditionNotes,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	// The warning rides on every update, not only on a status change. A
	// caller that has to ask "did I change the status?" to know whether to
	// read the warning is a caller that will get it wrong once (BR-013).
	//
	// Empty until S1-022 -- see catalog.AffectedBookings.
	affected, err := s.catalog.AffectedBookings(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	bookings := make([]AffectedBooking, 0, len(affected))
	for _, b := range affected {
		bookings = append(bookings, AffectedBooking{
			Code: b.Code, StartAt: b.StartAt, EndAt: b.EndAt, Status: b.Status,
		})
	}

	writeJSON(w, r, http.StatusOK, UnitUpdated{
		Id:             unit.ID,
		ResourceId:     unit.ResourceID,
		Code:           unit.Code,
		Label:          unit.Label,
		Status:         UnitStatus(unit.Status),
		MeterValue:     unit.MeterValue,
		ConditionNotes: unit.ConditionNotes,
		Warning:        UnitWarning{AffectedBookings: bookings},
	})
}

// DeleteUnit handles DELETE /units/{id}.
func (s *Server) DeleteUnit(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermDelete, func(w http.ResponseWriter, r *http.Request) {
		found, err := s.catalog.DeleteUnit(r.Context(), id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if !found {
			writeError(w, r, apperrors.NotFound("No such unit in this business."))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})(w, r)
}

func unitBody(unit catalog.Unit) ResourceUnit {
	return ResourceUnit{
		Id:             unit.ID,
		ResourceId:     unit.ResourceID,
		Code:           unit.Code,
		Label:          unit.Label,
		Status:         UnitStatus(unit.Status),
		MeterValue:     unit.MeterValue,
		ConditionNotes: unit.ConditionNotes,
	}
}

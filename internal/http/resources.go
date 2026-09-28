package httpapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	"github.com/miqbalhamdani/sewain-api/internal/catalog"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// The catalogue over HTTP.  (S1-014, S1-016)
//
// Nothing here mentions sqlc, and nothing here re-states a rule the database
// already enforces. What it does own is two things the database cannot see:
// which permission a caller needs, and -- for the four BR-016 nominals --
// whether a key was in the request body at all.

// ListResources handles GET /resources.
func (s *Server) ListResources(w http.ResponseWriter, r *http.Request) {
	requirePermission(auth.PermResourcesRead, s.listResources)(w, r)
}

func (s *Server) listResources(w http.ResponseWriter, r *http.Request) {
	rows, err := s.catalog.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	body := make([]Resource, 0, len(rows))
	for _, res := range rows {
		body = append(body, resourceBody(res))
	}
	writeJSON(w, r, http.StatusOK, body)
}

// GetResource handles GET /resources/{id}.
func (s *Server) GetResource(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermResourcesRead, func(w http.ResponseWriter, r *http.Request) {
		res, err := s.catalog.Get(r.Context(), id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, resourceBody(res))
	})(w, r)
}

// CreateResource handles POST /resources.
func (s *Server) CreateResource(w http.ResponseWriter, r *http.Request) {
	requirePermission(auth.PermResourcesWrite, s.createResource)(w, r)
}

func (s *Server) createResource(w http.ResponseWriter, r *http.Request) {
	raw, present, ok := readBody(w, r)
	if !ok {
		return
	}
	var body ResourceCreate
	if err := json.Unmarshal(raw, &body); err != nil {
		writeError(w, r, apperrors.ValidationFailed(
			"The request body is malformed or a field is not in the expected format.").WithCause(err))
		return
	}
	if err := refuseServerManaged(present); err != nil {
		writeError(w, r, err)
		return
	}
	if body.Name == "" {
		writeError(w, r, apperrors.ValidationFailed("name is required.").
			WithFields(apperrors.Field{Name: "name"}))
		return
	}

	userID, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(w, r, apperrors.Unauthenticated("A bearer token is required."))
		return
	}

	// On create, absent and null mean the same thing -- the rule does not
	// apply -- so plain pointers are enough. Only PATCH has to tell them
	// apart, because only PATCH can revoke (04-api-spec.md section 3.2).
	res, err := s.catalog.Create(r.Context(), userID, catalog.NewResource{
		Name:                   body.Name,
		Category:               body.Category,
		BasePrice:              body.BasePrice,
		DepositAmount:          body.DepositAmount,
		LateFeePerUnit:         body.LateFeePerUnit,
		MinDuration:            int32Ptr(body.MinDuration),
		MaxDuration:            int32Ptr(body.MaxDuration),
		BufferMinutes:          int32Or(body.BufferMinutes, 0),
		RequiresIDVerification: boolOr(body.RequiresIdVerification, false),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, resourceBody(res))
}

// UpdateResource handles PATCH /resources/{id}.
func (s *Server) UpdateResource(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermResourcesWrite, func(w http.ResponseWriter, r *http.Request) {
		s.updateResource(w, r, id)
	})(w, r)
}

func (s *Server) updateResource(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	raw, present, ok := readBody(w, r)
	if !ok {
		return
	}
	var body ResourceUpdate
	if err := json.Unmarshal(raw, &body); err != nil {
		writeError(w, r, apperrors.ValidationFailed(
			"The request body is malformed or a field is not in the expected format.").WithCause(err))
		return
	}
	if err := refuseServerManaged(present); err != nil {
		writeError(w, r, err)
		return
	}

	// BR-003 names the AMOUNTS, not the endpoint. Today an operator has no
	// resources:write either, so this check has never been the only thing
	// refusing -- it stays because it is the one that is still right if the
	// role matrix ever changes.
	if present["base_price"] != nil || present["deposit_amount"] != nil ||
		present["late_fee_per_unit"] != nil {
		role, _ := auth.RoleFromContext(r.Context())
		if !auth.Can(role, auth.PermPricingWrite) {
			writeError(w, r, apperrors.PermissionDenied(auth.PermPricingWrite))
			return
		}
	}

	res, err := s.catalog.Update(r.Context(), id, catalog.ResourcePatch{
		Name:                   body.Name,
		Category:               body.Category,
		BasePrice:              body.BasePrice,
		Status:                 stringPtr(body.Status),
		BufferMinutes:          int32Ptr(body.BufferMinutes),
		RequiresIDVerification: body.RequiresIdVerification,

		DepositAmount:  optional(present, "deposit_amount", body.DepositAmount),
		LateFeePerUnit: optional(present, "late_fee_per_unit", body.LateFeePerUnit),
		MinDuration:    optional(present, "min_duration", int32Ptr(body.MinDuration)),
		MaxDuration:    optional(present, "max_duration", int32Ptr(body.MaxDuration)),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	// Always 0 until S1-022. The field is here now so the screen that reads it
	// is written once -- see catalog.ActiveBookings.
	active, err := s.catalog.ActiveBookings(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, ResourceUpdated{
		Id:                     res.ID,
		Name:                   res.Name,
		Category:               res.Category,
		PricingUnit:            PricingUnit(res.PricingUnit),
		BasePrice:              res.BasePrice,
		DepositAmount:          res.DepositAmount,
		LateFeePerUnit:         res.LateFeePerUnit,
		MinDuration:            intPtr(res.MinDuration),
		MaxDuration:            intPtr(res.MaxDuration),
		BufferMinutes:          int(res.BufferMinutes),
		RequiresIdVerification: res.RequiresIDVerification,
		Status:                 ResourceStatus(res.Status),
		UnitCount:              int(res.UnitCount),
		ActiveBookings:         active,
	})
}

// DeleteResource handles DELETE /resources/{id}.
//
// records:delete rather than resources:write: BR-003 says an operator may not
// delete anything at all, and that blanket is most of why a juragan is willing
// to hand out an account in the first place.
func (s *Server) DeleteResource(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermDelete, func(w http.ResponseWriter, r *http.Request) {
		found, err := s.catalog.Delete(r.Context(), id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if !found {
			writeError(w, r, apperrors.NotFound("No such resource in this business."))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})(w, r)
}

// resourceBody is the domain type becoming the contract type. The two happen to
// have the same shape today; they are not the same thing, and the contract is
// free to rename a field without the database following.
func resourceBody(res catalog.Resource) Resource {
	return Resource{
		Id:                     res.ID,
		Name:                   res.Name,
		Category:               res.Category,
		PricingUnit:            PricingUnit(res.PricingUnit),
		BasePrice:              res.BasePrice,
		DepositAmount:          res.DepositAmount,
		LateFeePerUnit:         res.LateFeePerUnit,
		MinDuration:            intPtr(res.MinDuration),
		MaxDuration:            intPtr(res.MaxDuration),
		BufferMinutes:          int(res.BufferMinutes),
		RequiresIdVerification: res.RequiresIDVerification,
		Status:                 ResourceStatus(res.Status),
		UnitCount:              int(res.UnitCount),
	}
}

// readBody reads the request body once and returns it alongside its raw keys.
//
// Read once, decode twice. The generated types use *T for a nullable field, so
// `{"deposit_amount": null}` and `{}` both arrive as nil -- and under BR-016
// those are opposite instructions: the first revokes the deposit, the second
// leaves it alone. Nothing in the decoded struct survives that difference, so
// the keys are kept.
//
// It is also what makes a server-managed field refusable rather than silently
// dropped, which is the other thing a decoded struct cannot tell you.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, map[string]json.RawMessage, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeError(w, r, apperrors.ValidationFailed("The request body could not be read.").WithCause(err))
		return nil, nil, false
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal(raw, &present); err != nil {
		writeError(w, r, apperrors.ValidationFailed(
			"The request body is malformed or a field is not in the expected format.").WithCause(err))
		return nil, nil, false
	}
	return raw, present, true
}

// refuseServerManaged rejects a body that names a field the server owns.
//
// 422 rather than a silent drop, because 04-api-spec.md section 3.2 says so and
// because dropping it quietly is worse: a client that believed it set the
// pricing unit gets an invoice computed on a different one, and nothing in the
// exchange said no. The strongest guard is still that pricing_unit is absent
// from ResourceCreate and ResourceUpdate entirely, so a generated client cannot
// express it -- this catches the hand-rolled caller that can.
//
// owner_id and id are in the list for the same reason even though no schema has
// them: this is the boundary where a body stops being trusted (BR-001).
func refuseServerManaged(present map[string]json.RawMessage) error {
	for _, field := range []string{"pricing_unit", "id", "owner_id", "unit_count", "active_bookings"} {
		if _, sent := present[field]; sent {
			return apperrors.ValidationFailed(
				field + " is set by the server and cannot be sent.").
				WithFields(apperrors.Field{Name: field})
		}
	}
	return nil
}

// optional pairs a decoded value with whether its key was in the body at all.
//
// json.RawMessage for an absent key is nil; for `"x": null` it is the four
// bytes `null`. That is the only place in the decoded output where the
// difference survives.
func optional[T any](present map[string]json.RawMessage, key string, value *T) catalog.Optional[T] {
	_, ok := present[key]
	return catalog.Optional[T]{Set: ok, Value: value}
}

func int32Ptr(v *int) *int32 {
	if v == nil {
		return nil
	}
	n := int32(*v) //nolint:gosec // bounded by the schema's minimum and the column's CHECK
	return &n
}

func intPtr(v *int32) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

func int32Or(v *int, fallback int32) int32 {
	if v == nil {
		return fallback
	}
	return int32(*v) //nolint:gosec // bounded by the schema's minimum and the column's CHECK
}

func boolOr(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

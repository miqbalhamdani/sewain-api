package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// Account administration over HTTP.  (S1-010)
//
// All four are owner-only. An operator does not see this navigation at all in
// the UI, and reaching the endpoint anyway gets a 403 naming the permission --
// a disabled button that still 403s is the violation of BR-003, not the
// fulfilment of it.
//
// The id-taking pair answers 404, never 403, for an id belonging to another
// rental. Nothing in this file checks ownership: the queries run inside
// InOwnerTx, RLS makes the other rental's row invisible, and zero rows is the
// 404. A row that cannot be seen and a row that does not exist are the same
// answer on purpose (BR-001).

// ListUsers handles GET /users.
func (s *Server) ListUsers(w http.ResponseWriter, r *http.Request) {
	requirePermission(auth.PermUsersRead, s.listUsers)(w, r)
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.auth.ListUsers(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}

	// An empty array, never null: the contract types this as a list, and a
	// client that has to handle both shapes will eventually handle one wrong.
	body := make([]User, 0, len(rows))
	for _, row := range rows {
		body = append(body, User{
			Id: row.ID, Email: emailOf(row.Email), Name: row.Name,
			Role: UserRole(row.Role), Status: UserStatus(row.Status),
			LastLoginAt: row.LastLoginAt,
		})
	}
	writeJSON(w, r, http.StatusOK, body)
}

// InviteUser handles POST /users.
func (s *Server) InviteUser(w http.ResponseWriter, r *http.Request) {
	requirePermission(auth.PermUsersWrite, s.inviteUser)(w, r)
}

func (s *Server) inviteUser(w http.ResponseWriter, r *http.Request) {
	var body InviteUserRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, r, apperrors.ValidationFailed(
			"The request body is malformed or a field is not in the expected format.").WithCause(err))
		return
	}
	if body.Email == "" || body.Name == "" {
		writeError(w, r, apperrors.ValidationFailed("email and name are required."))
		return
	}

	// The contract's default, applied here rather than left to the database:
	// an absent role means operator, which is the whole point of the endpoint.
	role := auth.RoleOperator
	if body.Role != nil {
		role = string(*body.Role)
	}

	ownerID, ok := owner.FromContext(r.Context())
	if !ok {
		writeError(w, r, apperrors.Unauthenticated("A bearer token is required."))
		return
	}

	row, err := s.auth.InviteUser(r.Context(), ownerID, string(body.Email), body.Name, role)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, User{
		Id: row.ID, Email: emailOf(row.Email), Name: row.Name,
		Role: UserRole(row.Role), Status: UserStatus(row.Status),
		LastLoginAt: row.LastLoginAt,
	})
}

// UpdateUser handles PATCH /users/{id}.
func (s *Server) UpdateUser(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermUsersWrite, func(w http.ResponseWriter, r *http.Request) {
		s.updateUser(w, r, id)
	})(w, r)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	var body UpdateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, r, apperrors.ValidationFailed(
			"The request body is malformed or a field is not in the expected format.").WithCause(err))
		return
	}

	row, err := s.auth.UpdateUser(r.Context(), id, stringPtr(body.Role), statusPtr(body.Status))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, User{
		Id: row.ID, Email: emailOf(row.Email), Name: row.Name,
		Role: UserRole(row.Role), Status: UserStatus(row.Status),
		LastLoginAt: row.LastLoginAt,
	})
}

// DisableUser handles DELETE /users/{id}.
func (s *Server) DisableUser(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermUsersWrite, func(w http.ResponseWriter, r *http.Request) {
		s.disableUser(w, r, id)
	})(w, r)
}

func (s *Server) disableUser(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	if err := s.auth.DisableUser(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// The generated enums are distinct named string types per operation, so the
// three converters below exist to avoid repeating the cast at every call site.

func stringPtr[T ~string](v *T) *string {
	if v == nil {
		return nil
	}
	s := string(*v)
	return &s
}

func statusPtr(v *UpdateUserRequestStatus) *string { return stringPtr(v) }

func emailOf(s string) openapi_types.Email { return openapi_types.Email(s) }

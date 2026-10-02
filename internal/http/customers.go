package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	"github.com/miqbalhamdani/sewain-api/internal/customer"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// Renters over HTTP.  (S1-020, S1-021)

// ListCustomers handles GET /customers.
func (s *Server) ListCustomers(w http.ResponseWriter, r *http.Request, params ListCustomersParams) {
	requirePermission(auth.PermCustomersRead, func(w http.ResponseWriter, r *http.Request) {
		var cursor *uuid.UUID
		if params.Cursor != nil {
			id, err := uuid.Parse(*params.Cursor)
			if err != nil {
				writeError(w, r, badCursor())
				return
			}
			cursor = &id
		}
		var q *string
		if params.Q != nil && strings.TrimSpace(*params.Q) != "" {
			q = ptr(strings.TrimSpace(*params.Q))
		}
		rows, next, err := s.customers.List(r.Context(), q, params.Blacklisted, cursor, pageLimit(params.Limit))
		if err != nil {
			writeError(w, r, err)
			return
		}
		page := CustomerPage{Data: make([]Customer, 0, len(rows))}
		for _, c := range rows {
			page.Data = append(page.Data, customerBody(c))
		}
		if next != nil {
			page.NextCursor = ptr(next.String())
		}
		writeJSON(w, r, http.StatusOK, page)
	})(w, r)
}

// CreateCustomer handles POST /customers.
func (s *Server) CreateCustomer(w http.ResponseWriter, r *http.Request) {
	requirePermission(auth.PermCustomersWrite, func(w http.ResponseWriter, r *http.Request) {
		var body CustomerCreate
		if !decodeCustomer(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.Name) == "" || strings.TrimSpace(body.Phone) == "" {
			writeError(w, r, apperrors.ValidationFailed("name and phone are required.").
				WithFields(apperrors.Field{Name: "name"}, apperrors.Field{Name: "phone"}))
			return
		}
		userID, ok := auth.UserFromContext(r.Context())
		if !ok {
			writeError(w, r, apperrors.Unauthenticated("A bearer token is required."))
			return
		}
		c, err := s.customers.Create(r.Context(), userID, customer.Input{
			Name: ptr(strings.TrimSpace(body.Name)), Phone: ptr(strings.TrimSpace(body.Phone)),
			IDType: stringPtr(body.IdType), IDNumber: body.IdNumber,
		})
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusCreated, customerBody(c))
	})(w, r)
}

// GetCustomer handles GET /customers/{id}.
func (s *Server) GetCustomer(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermCustomersRead, func(w http.ResponseWriter, r *http.Request) {
		c, err := s.customers.Get(r.Context(), id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, customerBody(c))
	})(w, r)
}

// UpdateCustomer handles PATCH /customers/{id}.
func (s *Server) UpdateCustomer(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermCustomersWrite, func(w http.ResponseWriter, r *http.Request) {
		var body CustomerUpdate
		if !decodeCustomer(w, r, &body) {
			return
		}
		c, err := s.customers.Update(r.Context(), id, customer.Input{
			Name: body.Name, Phone: body.Phone, IDType: stringPtr(body.IdType), IDNumber: body.IdNumber,
		})
		if err != nil {
			writeError(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, customerBody(c))
	})(w, r)
}

// BlacklistCustomer handles POST /customers/{id}/blacklist. Owner only (BR-028).
func (s *Server) BlacklistCustomer(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermCustomersBlacklist, func(w http.ResponseWriter, r *http.Request) {
		var body BlacklistRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			writeError(w, r, malformed(err))
			return
		}
		reason := strings.TrimSpace(body.Reason)
		if reason == "" {
			writeError(w, r, apperrors.ValidationFailed(
				"A block needs a reason -- the operator who is refused reads it (BR-028).").
				WithFields(apperrors.Field{Name: "reason"}))
			return
		}
		s.setBlacklist(w, r, id, &reason)
	})(w, r)
}

// UnblacklistCustomer handles DELETE /customers/{id}/blacklist. Owner only.
func (s *Server) UnblacklistCustomer(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermCustomersBlacklist, func(w http.ResponseWriter, r *http.Request) {
		s.setBlacklist(w, r, id, nil)
	})(w, r)
}

func (s *Server) setBlacklist(w http.ResponseWriter, r *http.Request, id uuid.UUID, reason *string) {
	userID, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(w, r, apperrors.Unauthenticated("A bearer token is required."))
		return
	}
	c, err := s.customers.SetBlacklist(r.Context(), userID, id, reason)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, customerBody(c))
}

// decodeCustomer refuses the fields this body must never carry: the blacklist
// pair, which only /blacklist may set, and the server's own columns.
func decodeCustomer(w http.ResponseWriter, r *http.Request, into any) bool {
	raw, present, ok := readBody(w, r)
	if !ok {
		return false
	}
	for _, field := range []string{"id", "owner_id", "is_blacklisted", "blacklist_reason", "id_number_last4"} {
		if _, sent := present[field]; sent {
			writeError(w, r, apperrors.ValidationFailed(field+" cannot be sent here.").
				WithFields(apperrors.Field{Name: field}))
			return false
		}
	}
	if err := json.Unmarshal(raw, into); err != nil {
		writeError(w, r, malformed(err))
		return false
	}
	return true
}

func customerBody(c customer.Customer) Customer {
	return Customer{
		Id: c.ID, Name: c.Name, Phone: c.Phone, IdType: (*IdType)(c.IDType),
		IdNumberLast4: c.IDNumberLast4, IsBlacklisted: c.IsBlacklisted,
		BlacklistReason: c.BlacklistReason, CreatedAt: c.CreatedAt,
	}
}

func malformed(err error) error {
	return apperrors.ValidationFailed(
		"The request body is malformed or a field is not in the expected format.").WithCause(err)
}

func badCursor() error {
	return apperrors.ValidationFailed("cursor is not one this API issued.").
		WithFields(apperrors.Field{Name: "cursor"})
}

// pageLimit applies the contract's default and bounds (components.parameters.Limit).
func pageLimit(limit *Limit) int {
	switch {
	case limit == nil:
		return 50
	case *limit < 1:
		return 1
	case *limit > 200:
		return 200
	}
	return *limit
}

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/miqbalhamdani/sewain-api/internal/booking"
	"github.com/miqbalhamdani/sewain-api/internal/catalog"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
	"github.com/miqbalhamdani/sewain-api/internal/public"
)

// The public surface over HTTP.  (S1-051, 04-api-spec.md §4)
//
// Every route here runs behind Lanes: the owner in the context came from Host
// (or an API key on api.<apex>), and the page is live. What this file adds is
// the trimming -- no unit code, no unit id, no renter, no owner_id.

// GetPublicOwner handles GET /public/owner.
func (s *Server) GetPublicOwner(w http.ResponseWriter, r *http.Request) {
	p, err := s.bookings.Profile(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, PublicOwner{Name: p.Name, Whatsapp: derefStr(p.WhatsApp),
		Address: derefStr(p.Address), OperatingHours: p.OperatingHours})
}

// ListPublicResources handles GET /public/resources.
func (s *Server) ListPublicResources(w http.ResponseWriter, r *http.Request, params ListPublicResourcesParams) {
	avail, ok := s.publicAvailability(w, r, params.StartAt, params.EndAt, nil)
	if !ok {
		return
	}
	res, err := s.catalog.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	data := []PublicResource{}
	for _, x := range res {
		if listed(x) {
			data = append(data, publicResource(x, avail))
		}
	}
	writeJSON(w, r, http.StatusOK, PublicResourceList{Data: data})
}

// GetPublicResource handles GET /public/resources/{id}.
func (s *Server) GetPublicResource(w http.ResponseWriter, r *http.Request, id uuid.UUID, params GetPublicResourceParams) {
	x, err := s.catalog.Get(r.Context(), id)
	if err == nil && !listed(x) {
		err = notFoundPublic()
	}
	if err != nil {
		writeError(w, r, publicErr(err))
		return
	}
	avail, ok := s.publicAvailability(w, r, params.StartAt, params.EndAt, &id)
	if !ok {
		return
	}
	k, err := s.settings.Get(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	base := publicResource(x, avail)
	writeJSON(w, r, http.StatusOK, PublicResourceDetail{
		Id: base.Id, Name: base.Name, Category: base.Category, PricingUnit: base.PricingUnit,
		BasePrice: base.BasePrice, DepositAmount: base.DepositAmount,
		MinDuration: base.MinDuration, MaxDuration: base.MaxDuration, Vehicle: base.Vehicle,
		Available: base.Available, AvailableCount: base.AvailableCount,
		DurationQty: base.DurationQty, Subtotal: base.Subtotal,
		Description: x.Description, TermsExcludes: x.TermsExcludes,
		TermsRequirements: x.TermsRequirements, TermsCancellation: x.TermsCancellation,
		LateFeePerUnit: x.LateFeePerUnit,
		SystemTerms: public.SystemTerms(x.PricingUnit, x.LateFeePerUnit, public.Knobs{
			PaymentDueHours: k.PaymentDueHours, NoShowToleranceHours: k.NoShowToleranceHours,
			RequirePaymentBeforePickup: k.RequirePaymentBeforePickup}),
	})
}

var publicPhone = regexp.MustCompile(`^(\+62|0)[0-9]{8,13}$`)

// CreatePublicBooking handles POST /public/bookings. Idempotency-Key is the
// middleware's (BR-090); the rate limit is Lanes'.
func (s *Server) CreatePublicBooking(w http.ResponseWriter, r *http.Request, _ CreatePublicBookingParams) {
	var body PublicBookingRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	// Unknown fields are refused, not dropped: a body naming owner_id is an
	// attempt, not a typo (04-api-spec.md §1).
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, r, malformed(err))
		return
	}
	body.Customer.Name = strings.TrimSpace(body.Customer.Name)
	if body.Customer.Name == "" || len(body.Customer.Name) > 100 {
		writeError(w, r, apperrors.ValidationFailed("customer.name is required.").
			WithFields(apperrors.Field{Name: "customer.name"}))
		return
	}
	if !publicPhone.MatchString(body.Customer.Phone) {
		writeError(w, r, apperrors.ValidationFailed("customer.phone starts with 0 or +62, then 8 to 13 digits.").
			WithFields(apperrors.Field{Name: "customer.phone"}))
		return
	}
	x, err := s.catalog.Get(r.Context(), body.ResourceId)
	if err == nil && !listed(x) {
		err = notFoundPublic()
	}
	if err != nil {
		writeError(w, r, publicErr(err))
		return
	}
	customerID, err := s.customers.FindOrCreateByPhone(r.Context(), body.Customer.Name, body.Customer.Phone)
	if err != nil {
		writeError(w, r, err)
		return
	}
	b, err := s.bookings.CreateDraft(r.Context(), booking.NewDraft{
		ResourceID: body.ResourceId, CustomerID: customerID, StartAt: body.StartAt, EndAt: body.EndAt})
	var known *apperrors.Error
	if errors.As(err, &known) && known.Code == apperrors.CodeCustomerBlacklisted {
		err = apperrors.CustomerBlacklistedPublic() // BR-028: neutral, no reason
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	ownerID, _ := owner.FromContext(r.Context())
	created := PublicBookingCreated{Code: b.Code, Status: PublicBookingCreatedStatusDraft, ExpiresAt: derefTime(b.ExpiresAt)}
	if u := s.portal.URL(ownerID, b.ID, b.OwnerSlug); u != nil {
		created.TrackUrl = *u
	}
	writeJSON(w, r, http.StatusCreated, created)
}

// publicAvailability reads availability when the query names a range: both
// ends or neither. nil map, ok = no range asked for.
func (s *Server) publicAvailability(w http.ResponseWriter, r *http.Request, start, end *time.Time,
	resourceID *uuid.UUID) (map[uuid.UUID]booking.Availability, bool) {
	if start == nil && end == nil {
		return nil, true
	}
	if start == nil || end == nil {
		writeError(w, r, apperrors.ValidationFailed("start_at and end_at come together.").
			WithFields(apperrors.Field{Name: "end_at"}))
		return nil, false
	}
	list, err := s.bookings.Availability(r.Context(), *start, *end, resourceID)
	if err != nil {
		writeError(w, r, err)
		return nil, false
	}
	out := make(map[uuid.UUID]booking.Availability, len(list))
	for _, a := range list {
		out[a.ResourceID] = a
	}
	return out, true
}

// listed is BR-025's gate: an active resource with at least one active unit.
func listed(x catalog.Resource) bool { return x.Status == "active" && x.UnitCount > 0 }

func notFoundPublic() error { return apperrors.NotFound("Not found.") }

// publicErr keeps every not-found on the public surface the same answer: the
// catalogue's own wording ("No such resource in this business") would tell a
// stranger which host is a real business (BR-030).
func publicErr(err error) error {
	var known *apperrors.Error
	if errors.As(err, &known) && known.Code == apperrors.CodeNotFound {
		return notFoundPublic()
	}
	return err
}

func publicResource(x catalog.Resource, avail map[uuid.UUID]booking.Availability) PublicResource {
	p := PublicResource{Id: x.ID, Name: x.Name, Category: x.Category, PricingUnit: PricingUnit(x.PricingUnit),
		BasePrice: x.BasePrice, DepositAmount: x.DepositAmount,
		MinDuration: intPtr(x.MinDuration), MaxDuration: intPtr(x.MaxDuration),
		Vehicle: vehicleSpecBody(x.Vehicle)}
	if avail != nil {
		a, ok := avail[x.ID]
		n := len(a.Units)
		free := ok && n > 0
		qty, sub := int(a.DurationQty), a.Subtotal
		p.Available, p.AvailableCount = &free, &n
		if ok {
			p.DurationQty, p.Subtotal = &qty, &sub
		}
	}
	return p
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

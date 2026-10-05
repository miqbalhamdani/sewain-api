package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/miqbalhamdani/sewain-api/internal/apikey"
	"github.com/miqbalhamdani/sewain-api/internal/auth"
	"github.com/miqbalhamdani/sewain-api/internal/booking"
	"github.com/miqbalhamdani/sewain-api/internal/catalog"
	"github.com/miqbalhamdani/sewain-api/internal/customer"
	"github.com/miqbalhamdani/sewain-api/internal/jobs"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
	"github.com/miqbalhamdani/sewain-api/internal/platform/ratelimit"
	"github.com/miqbalhamdani/sewain-api/internal/settings"
	"github.com/miqbalhamdani/sewain-api/internal/storage"
)

// refreshCookieName is the only place the refresh token lives on a client.
const refreshCookieName = "refresh_token"

// Server implements the generated ServerInterface.
//
// It is deliberately thin: decode, call internal/auth, encode. Anything that
// looks like a decision belongs in the service, where it can be tested without
// an HTTP request.
type Server struct {
	auth      *auth.Service
	settings  *settings.Service
	catalog   *catalog.Service
	customers *customer.Service
	bookings  *booking.Service
	objects   *storage.Store
	jobs      *jobs.Queue
	limiter   *ratelimit.Limiter
	portal    booking.PortalLinks // S1-053: portal tokens and links
	keys      *apikey.Service     // S1-079

	// secureCookies is false only for local development over plain HTTP, where
	// a Secure cookie would be dropped by the browser and nothing would work.
	secureCookies bool
}

func NewServer(authSvc *auth.Service, settingsSvc *settings.Service, catalogSvc *catalog.Service,
	customerSvc *customer.Service, bookingSvc *booking.Service, objects *storage.Store,
	jobQueue *jobs.Queue, limiter *ratelimit.Limiter, secureCookies bool) *Server {
	return &Server{
		auth:          authSvc,
		settings:      settingsSvc,
		catalog:       catalogSvc,
		customers:     customerSvc,
		bookings:      bookingSvc,
		objects:       objects,
		jobs:          jobQueue,
		limiter:       limiter,
		secureCookies: secureCookies,
	}
}

// WithPublic wires the M5 surfaces: portal links (S1-053) and API keys (S1-079).
func (s *Server) WithPublic(portal booking.PortalLinks, keys *apikey.Service) *Server {
	s.portal, s.keys = portal, keys
	return s
}

// Login handles POST /auth/login.
func (s *Server) Login(w http.ResponseWriter, r *http.Request) {
	var body LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// Decoding fails for malformed JSON and for a field the generated
		// type rejects -- an unparseable email reaches here, not the check
		// below. Saying "not valid JSON" to that is misleading.
		writeError(w, r, apperrors.ValidationFailed(
			"The request body is malformed or a field is not in the expected format.").WithCause(err))
		return
	}
	if body.Email == "" || len(body.Password) < 8 {
		writeError(w, r, apperrors.ValidationFailed(
			"email and password are required; password is at least 8 characters."))
		return
	}

	session, err := s.auth.Login(r.Context(), string(body.Email), body.Password)
	if err != nil {
		s.writeAuthError(w, r, err)
		return
	}
	s.writeSession(w, r, session)
}

// Refresh handles POST /auth/refresh.
func (s *Server) Refresh(w http.ResponseWriter, r *http.Request) {
	presented, _ := r.Cookie(refreshCookieName)
	if presented == nil {
		s.writeAuthError(w, r, auth.ErrUnauthenticated)
		return
	}

	session, err := s.auth.Refresh(r.Context(), presented.Value)
	if err != nil {
		// Clear the cookie on the way out. If the token was reused, every
		// session is now revoked and the browser holding this one should stop
		// presenting it.
		http.SetCookie(w, s.clearRefreshCookie())
		s.writeAuthError(w, r, err)
		return
	}
	s.writeSession(w, r, session)
}

// Logout handles POST /auth/logout.
func (s *Server) Logout(w http.ResponseWriter, r *http.Request) {
	var presented string
	if c, _ := r.Cookie(refreshCookieName); c != nil {
		presented = c.Value
	}
	if err := s.auth.Logout(r.Context(), presented); err != nil {
		writeError(w, r, err)
		return
	}
	http.SetCookie(w, s.clearRefreshCookie())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeSession(w http.ResponseWriter, r *http.Request, session auth.Session) {
	s.writeSessionStatus(w, r, session, http.StatusOK)
}

// writeSessionStatus exists because register answers 201 and everything else
// answers 200. The body and the cookie are identical either way.
func (s *Server) writeSessionStatus(w http.ResponseWriter, r *http.Request, session auth.Session, status int) {
	http.SetCookie(w, s.refreshCookie(session.RefreshToken, auth.RefreshTokenTTL))

	writeJSON(w, r, status, Session{
		AccessToken: session.AccessToken,
		ExpiresIn:   session.ExpiresIn,
		User: SessionUser{
			Id:              session.User.ID,
			Name:            session.User.Name,
			Role:            SessionUserRole(session.User.Role),
			EmailVerifiedAt: session.User.EmailVerifiedAt,
			Permissions:     session.User.Permissions,
		},
		Owner: SessionOwner{
			Id:           session.Owner.ID,
			Name:         session.Owner.Name,
			Slug:         session.Owner.Slug,
			BusinessType: BusinessType(session.Owner.BusinessType),
		},
	})
}

// writeAuthError collapses every authentication failure into one answer.
//
// A wrong password, an unknown email, a disabled account and a stolen token are
// deliberately indistinguishable to a client -- any difference tells an
// attacker which emails exist. The real cause still reaches the log through the
// trace id.
func (s *Server) writeAuthError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, auth.ErrUnauthenticated) {
		writeError(w, r, apperrors.Unauthenticated("Email or password is incorrect.").WithCause(err))
		return
	}
	writeError(w, r, err)
}

// refreshCookie builds the cookie the refresh token travels in.
//
// httpOnly so no script on the page can read it -- that is the whole reason the
// token is not in the response body. SameSite=Lax so it is not sent on a
// cross-site POST. Path is the auth routes only, so it is not attached to every
// request to the API.
func (s *Server) refreshCookie(value string, ttl time.Duration) *http.Cookie {
	return &http.Cookie{
		Name:     refreshCookieName,
		Value:    value,
		Path:     "/api/v1/auth",
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(ttl),
	}
}

func (s *Server) clearRefreshCookie() *http.Cookie {
	c := s.refreshCookie("", 0)
	c.MaxAge = -1
	c.Expires = time.Unix(0, 0)
	return c
}

// GetMe answers who the caller is, which rental they work in, and what their
// role permits. (GET /me)
//
// The permissions list is part of the UI contract, not a convenience: the client
// hides actions it does not find here rather than rendering them disabled, so a
// missing entry removes a button and a wrong one advertises a capability that
// will 403 (BR-003).
func (s *Server) GetMe(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserFromContext(r.Context())
	if !ok {
		// No user in the context means the request never passed authentication.
		writeError(w, r, apperrors.Unauthenticated("A bearer token is required."))
		return
	}

	user, ownerRow, err := s.auth.Me(r.Context(), userID)
	if err != nil {
		s.writeAuthError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, Me{
		User: SessionUser{
			Id:              user.ID,
			Name:            user.Name,
			Role:            SessionUserRole(user.Role),
			EmailVerifiedAt: user.EmailVerifiedAt,
			Permissions:     user.Permissions,
		},
		Owner: SessionOwner{Id: ownerRow.ID, Name: ownerRow.Name, Slug: ownerRow.Slug,
			BusinessType: BusinessType(ownerRow.BusinessType)},
	})
}

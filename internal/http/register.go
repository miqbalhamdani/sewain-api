package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/miqbalhamdani/sewain-api/internal/auth"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// Registration and email verification over HTTP.  (S1-082, S1-084)

// Register handles POST /auth/register.
//
// Rate limited per IP before anything else: it is unauthenticated, and every
// success creates a business (§7, BR-005).
func (s *Server) Register(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, "register:"+clientIP(r), registerPerHour, time.Hour) {
		return
	}

	var body RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, r, apperrors.ValidationFailed(
			"The request body is malformed or a field is not in the expected format.").WithCause(err))
		return
	}

	session, err := s.auth.Register(r.Context(),
		string(body.Email), body.Password, body.BusinessName, string(body.BusinessType))
	if err != nil {
		writeError(w, r, err)
		return
	}

	// Best effort, and logged rather than failed: the account exists, the
	// session works, and resend is one button away on the wall they are about
	// to land on. Rolling back a created business because a mail server
	// hiccuped would be worse than a retry the owner can trigger themselves.
	if err := s.auth.SendVerification(r.Context(), session.User.ID, session.Owner.ID); err != nil {
		slog.WarnContext(r.Context(), "verification mail not sent",
			"error", err, "user_id", session.User.ID)
	}

	s.writeSessionStatus(w, r, session, http.StatusCreated)
}

// VerifyEmail handles POST /auth/verify-email.
func (s *Server) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeError(w, r, apperrors.ValidationFailed("token is required."))
		return
	}
	if err := s.auth.VerifyEmail(r.Context(), body.Token); err != nil {
		writeError(w, r, err)
		return
	}
	// 204, and the client calls /auth/refresh next: the verified flag rides in
	// the access token, so the one it is holding still says unverified.
	w.WriteHeader(http.StatusNoContent)
}

// ResendVerification handles POST /auth/verify-email/resend.
//
// Authenticated, unlike the other three: the caller already has a session --
// that is exactly why register issues one -- and the limit is per user rather
// than per IP (§7).
func (s *Server) ResendVerification(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(w, r, apperrors.Unauthenticated("A bearer token is required."))
		return
	}
	ownerID, ok := owner.FromContext(r.Context())
	if !ok {
		writeError(w, r, apperrors.Unauthenticated("A bearer token is required."))
		return
	}
	if !s.allow(w, r, "resend:"+userID.String(), resendPerHour, time.Hour) {
		return
	}

	if err := s.auth.ResendVerification(r.Context(), userID, ownerID); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AcceptInvitation handles POST /auth/accept-invitation.
func (s *Server) AcceptInvitation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeError(w, r, apperrors.ValidationFailed("token and password are required."))
		return
	}

	session, err := s.auth.AcceptInvitation(r.Context(), body.Token, body.Password)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// 200, not 201: the account existed before this call. What changed is that
	// it became usable.
	s.writeSessionStatus(w, r, session, http.StatusOK)
}

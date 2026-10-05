package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/miqbalhamdani/sewain-api/internal/apikey"
	"github.com/miqbalhamdani/sewain-api/internal/auth"
)

// API keys over HTTP.  (S1-079, BR-031)
//
// Owner only, through settings:write -- the key opens the business's catalogue
// to someone else's website, which is a settings decision.

// ListApiKeys handles GET /api-keys.
func (s *Server) ListApiKeys(w http.ResponseWriter, r *http.Request) {
	requirePermission(auth.PermSettingsWrite, func(w http.ResponseWriter, r *http.Request) {
		keys, err := s.keys.List(r.Context())
		if err != nil {
			writeError(w, r, err)
			return
		}
		out := make([]ApiKey, 0, len(keys))
		for _, k := range keys {
			out = append(out, apiKeyBody(k))
		}
		writeJSON(w, r, http.StatusOK, out)
	})(w, r)
}

// CreateApiKey handles POST /api-keys. The secret is in this response and
// nowhere else, ever.
func (s *Server) CreateApiKey(w http.ResponseWriter, r *http.Request) {
	requirePermission(auth.PermSettingsWrite, func(w http.ResponseWriter, r *http.Request) {
		var body ApiKeyCreate
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeError(w, r, malformed(err))
			return
		}
		perMin := 0
		if body.RateLimitPerMin != nil {
			perMin = *body.RateLimitPerMin
		}
		actor, _ := auth.UserFromContext(r.Context())
		k, secret, err := s.keys.Create(r.Context(), actor, body.Name, perMin)
		if err != nil {
			writeError(w, r, err)
			return
		}
		b := apiKeyBody(k)
		writeJSON(w, r, http.StatusCreated, ApiKeyCreated{Id: b.Id, Name: b.Name, KeyPrefix: b.KeyPrefix,
			RateLimitPerMin: b.RateLimitPerMin, LastUsedAt: b.LastUsedAt, RevokedAt: b.RevokedAt,
			CreatedAt: b.CreatedAt, Secret: secret})
	})(w, r)
}

// RevokeApiKey handles DELETE /api-keys/{id}: revoked, never deleted.
func (s *Server) RevokeApiKey(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requirePermission(auth.PermSettingsWrite, func(w http.ResponseWriter, r *http.Request) {
		if err := s.keys.Revoke(r.Context(), id); err != nil {
			writeError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})(w, r)
}

func apiKeyBody(k apikey.Key) ApiKey {
	return ApiKey{Id: k.ID, Name: k.Name, KeyPrefix: k.Prefix, RateLimitPerMin: k.RateLimitPerMin,
		LastUsedAt: k.LastUsedAt, RevokedAt: k.RevokedAt, CreatedAt: k.CreatedAt}
}

package httpapi

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// Rate limits at the handler boundary.  (§7)
//
// The numbers are here rather than in internal/platform/ratelimit because §7
// ends with "Angka-angka ini konfigurasi, bukan konstanta di kode" -- they will
// move to config, and having them in one file is what makes that a small
// change rather than a hunt.
const (
	registerPerHour int64 = 5 // unauthenticated, and each success creates a business
	resendPerHour   int64 = 3 // per user; it is a mail sender, not a data endpoint
)

// allow reports whether the request fits under the limit, and writes the 429
// itself when it does not.
//
// Returning a bool rather than an error so the call site reads as a guard:
// `if !s.allow(...) { return }`.
func (s *Server) allow(w http.ResponseWriter, r *http.Request, key string, limit int64, window time.Duration) bool {
	ok, retryAfter, err := s.limiter.Allow(r.Context(), key, limit, window)
	if err != nil {
		// Allow returns true on a Redis failure by design -- refusing every
		// registration because a cache is down turns a degraded dependency
		// into an outage on the one endpoint that creates customers.
		slog.WarnContext(r.Context(), "rate limiter unavailable, allowing request", "error", err)
		return true
	}
	if ok {
		return true
	}

	w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	writeError(w, r, apperrors.RateLimited(
		"Too many attempts. Try again in "+retryAfter.String()+"."))
	return false
}

// clientIP is the address a per-IP limit counts against.
//
// RemoteAddr, not X-Forwarded-For. The header is trivially forged, and
// trusting it unconditionally turns a 5-per-hour limit into no limit at all
// for anyone who sets one header. When the proxy lands (S1-073) this reads the
// hop it configures -- and that is a change made deliberately, with the proxy
// in front of it, rather than an assumption made early.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

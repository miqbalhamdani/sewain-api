package httpapi

import (
	"net"
	"net/http"
	"time"

	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// Rate limits at the handler boundary.  (§7)
//
// The numbers are here rather than in internal/platform/ratelimit because §7
// ends with "Angka-angka ini konfigurasi, bukan konstanta di kode" -- they will
// move to config, and having them in one file is what makes that a small
// change rather than a hunt.
// resendPerHour is per user; it is a mail sender, not a data endpoint.
const resendPerHour int64 = 3

// registerPerHour is per IP: unauthenticated, and each success creates a
// business. Configuration (REGISTER_PER_HOUR_IP), because behind one proxy --
// the end-to-end suite's Caddy -- every caller shares an address.
var registerPerHour = config.RegisterPerHourIP()

// allow reports whether the request fits under the limit, and writes the 429
// itself when it does not.
//
// Returning a bool rather than an error so the call site reads as a guard:
// `if !s.allow(...) { return }`.
func (s *Server) allow(w http.ResponseWriter, r *http.Request, key string, limit int64, window time.Duration) bool {
	return allowRate(w, r, s.limiter, key, limit, window, func(wait string) *apperrors.Error {
		return apperrors.RateLimited("Too many attempts. Try again in " + wait + ".")
	})
}

// clientIP is the address a per-IP limit counts against.
//
// RemoteAddr, not X-Forwarded-For. The header is trivially forged, and
// trusting it unconditionally turns a 5-per-hour limit into no limit at all
// for anyone who sets one header. When the proxy lands (S1-073) this reads the
// hop it configures -- and that is a change made deliberately, with the proxy
// in front of it, rather than an assumption made early.
//
// Behind Caddy (S1-051) it is X-Real-IP -- but only once Lanes has checked the
// proxy secret and put it in the context. The header alone is never trusted.
func clientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

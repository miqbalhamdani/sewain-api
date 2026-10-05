package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/miqbalhamdani/sewain-api/internal/apikey"
	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/owner"
	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
	"github.com/miqbalhamdani/sewain-api/internal/platform/ratelimit"
)

// Lanes routes a request by its Host.  (S1-051, S1-080, BR-030, BR-032)
//
//	api.<apex>     external: four /public/* routes, tenant from X-API-Key
//	<slug>.<apex>  tenant:   /public/* and /portal/*, tenant from Host
//	anything else  backoffice: everything except /public/* and /portal/*
//
// A path outside its lane is the uniform 404 -- the same answer as an unknown
// host, a suspended owner, or another owner's row. The two tokenless lanes
// also require Caddy's X-Proxy-Secret: Host is only trustworthy when the proxy
// set it, and a forged Host sent straight to the port must not name a tenant.
// Never a third lane (BR-030).
type Lanes struct {
	Apex        string
	ProxySecret string
	Store       *db.Store
	Keys        *apikey.Service
	Limiter     *ratelimit.Limiter
	Limits      config.PublicLimits
}

type lane int

const (
	laneBackoffice lane = iota
	laneTenant
	laneExternal
)

const proxySecretHeader = "X-Proxy-Secret"

func (l Lanes) classify(host string) (lane, string) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	if l.Apex == "" || !strings.HasSuffix(host, "."+l.Apex) {
		return laneBackoffice, ""
	}
	label := strings.TrimSuffix(host, "."+l.Apex)
	switch label {
	case "api":
		return laneExternal, ""
	case "app", "www":
		return laneBackoffice, ""
	}
	return laneTenant, label
}

func (l Lanes) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publicPath := strings.HasPrefix(r.URL.Path, BasePath+"/public/")
		portalPath := strings.HasPrefix(r.URL.Path, BasePath+"/portal/")
		kind, slug := l.classify(r.Host)
		if kind == laneBackoffice {
			if publicPath || portalPath {
				writeError(w, r, notFoundPublic())
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		got := r.Header.Get(proxySecretHeader)
		if l.ProxySecret == "" || subtle.ConstantTimeCompare([]byte(got), []byte(l.ProxySecret)) != 1 {
			writeError(w, r, notFoundPublic())
			return
		}
		// Only behind the secret is X-Real-IP Caddy's word, not the caller's.
		ctx := r.Context()
		if ip := r.Header.Get("X-Real-IP"); ip != "" {
			ctx = context.WithValue(ctx, clientIPKey{}, ip)
		}
		r = r.WithContext(ctx)

		switch kind {
		case laneTenant:
			l.tenant(w, r, next, slug, publicPath, portalPath)
		case laneExternal:
			l.external(w, r, next, publicPath)
		}
	})
}

// tenant: Host names the owner. X-API-Key is ignored -- it never changes the
// tenant here (BR-032).
func (l Lanes) tenant(w http.ResponseWriter, r *http.Request, next http.Handler, slug string, publicPath, portalPath bool) {
	if !publicPath && !portalPath {
		writeError(w, r, notFoundPublic())
		return
	}
	o, err := l.Store.OwnerBySlug(r.Context(), slug)
	if err != nil || !o.Active || (publicPath && !o.Live) {
		writeError(w, r, notFoundPublic())
		return
	}
	ip := clientIP(r)
	switch {
	case portalPath:
		if !l.allow(w, r, "portal:"+portalToken(r.URL.Path), l.Limits.PortalPerMinToken, time.Minute, rateLimited) {
			return
		}
	case r.Method == http.MethodPost:
		if !l.allow(w, r, "pubpost:ip:"+ip, l.Limits.PostPerHourIP, time.Hour, rateLimited) ||
			!l.allow(w, r, "pubpost:owner:"+o.ID.String(), l.Limits.PostPerHourOwner, time.Hour, rateLimited) {
			return
		}
	default:
		if !l.allow(w, r, "pub:ip:"+ip, l.Limits.GetPerMinIP, time.Minute, rateLimited) ||
			!l.allow(w, r, "pub:owner:"+o.ID.String(), l.Limits.GetPerMinOwner, time.Minute, rateLimited) {
			return
		}
	}
	next.ServeHTTP(w, r.WithContext(owner.NewContext(r.Context(), o.ID)))
}

// external: the key names the owner; Host is never a tenant source (BR-032).
func (l Lanes) external(w http.ResponseWriter, r *http.Request, next http.Handler, publicPath bool) {
	origin := r.Header.Get("Origin")
	w.Header().Add("Vary", "Origin") // permissions differ per Origin; caches must not share them

	if r.Method == http.MethodOptions { // preflight: browsers send no key here
		ok, err := l.Store.OriginAllowed(r.Context(), nil, origin)
		if err != nil || !ok || !publicPath {
			writeError(w, r, apperrors.OriginNotAllowed())
			return
		}
		allowCORS(w, origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "X-API-Key, Content-Type, Idempotency-Key")
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !publicPath || l.Keys == nil {
		writeError(w, r, notFoundPublic())
		return
	}
	key, err := l.Keys.Resolve(r.Context(), r.Header.Get("X-API-Key"))
	if errors.Is(err, apikey.ErrInvalid) {
		writeError(w, r, apperrors.InvalidAPIKey())
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	if origin != "" {
		ok, err := l.Store.OriginAllowed(r.Context(), &key.OwnerID, origin)
		if err != nil || !ok {
			writeError(w, r, apperrors.OriginNotAllowed())
			return
		}
		allowCORS(w, origin)
	}
	o, err := l.Store.OwnerByID(r.Context(), key.OwnerID)
	if err != nil || !o.Active || !o.Live {
		writeError(w, r, notFoundPublic())
		return
	}
	if !l.allow(w, r, "apikey:"+key.KeyID.String(), int64(key.RateLimitPerMin), time.Minute, quotaExceeded) {
		return
	}
	next.ServeHTTP(w, r.WithContext(owner.NewContext(r.Context(), key.OwnerID)))
}

func allowCORS(w http.ResponseWriter, origin string) {
	w.Header().Set("Access-Control-Allow-Origin", origin)
}

func (l Lanes) allow(w http.ResponseWriter, r *http.Request, key string, limit int64, window time.Duration,
	refuse func(string) *apperrors.Error) bool {
	return allowRate(w, r, l.Limiter, key, limit, window, refuse)
}

func rateLimited(wait string) *apperrors.Error {
	return apperrors.RateLimited("Too many requests. Try again in " + wait + ".")
}

func quotaExceeded(wait string) *apperrors.Error {
	return apperrors.QuotaExceeded("This key's per-minute quota is used up. Try again in " + wait + ".")
}

// portalToken is the {token} segment of /api/v1/portal/bookings/{token}/...
func portalToken(path string) string {
	rest := strings.TrimPrefix(path, BasePath+"/portal/bookings/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

type clientIPKey struct{}

// allowRate is the one place a limit is checked and a 429 written. A Redis
// failure allows the request: refusing everyone because a cache is down turns
// a degraded dependency into an outage.
func allowRate(w http.ResponseWriter, r *http.Request, limiter *ratelimit.Limiter, key string, limit int64,
	window time.Duration, refuse func(string) *apperrors.Error) bool {
	ok, retryAfter, err := limiter.Allow(r.Context(), key, limit, window)
	if err != nil {
		slog.WarnContext(r.Context(), "rate limiter unavailable, allowing request", "error", err)
		return true
	}
	if ok {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	writeError(w, r, refuse(retryAfter.String()))
	return false
}

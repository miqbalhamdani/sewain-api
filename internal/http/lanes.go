package httpapi

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

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

func (l Lanes) allow(w http.ResponseWriter, r *http.Request, key string, limit int64, window time.Duration,
	refuse func(string) *apperrors.Error) bool {
	return allowRate(w, r, l.Limiter, key, limit, window, refuse)
}

func rateLimited(wait string) *apperrors.Error {
	return apperrors.RateLimited("Too many requests. Try again in " + wait + ".")
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

package httpapi_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/owner"
	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
)

// M5 phase B and C over HTTP: lanes, the public surface, the portal, API keys.
// (S1-051, S1-053, S1-079, S1-080)

type harness struct {
	t   *testing.T
	srv http.Handler
}

func (h harness) do(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, r)
	return w
}

// noTrace drops what may differ between two otherwise identical problems:
// the trace id and the instance (the request path).
func noTrace(body string) string {
	body = strip(body, "\x00")
	if i := strings.Index(body, `"instance"`); i >= 0 {
		if j := strings.Index(body[i:], ","); j >= 0 {
			body = body[:i] + body[i+j+1:]
		}
	}
	return body
}

const (
	future     = "start_at=2030-01-10T09:00:00%2B07:00&end_at=2030-01-12T09:00:00%2B07:00"
	futureBody = `"start_at":"2030-01-10T09:00:00+07:00","end_at":"2030-01-12T09:00:00+07:00"`
)

// S1-051 + BR-030: every way of not being a live tenant answers the same 404,
// and a forged Host without the proxy names nobody.
func TestPublicLanesUniform404(t *testing.T) {
	ctx := t.Context()
	store := openAppStore(ctx, t)
	h := harness{t, newServer(t)}
	a := seedPublicOwner(ctx, t, store, uuid.Must(uuid.NewV7()))

	m := expect(t, h.do(tenantRequest(t, http.MethodGet, "/api/v1/public/owner", a.host, "")), http.StatusOK, "")
	if m["whatsapp"] != "+628123456789" || m["owner_id"] != nil {
		t.Errorf("owner profile = %v", m)
	}

	suspendedID := uuid.Must(uuid.NewV7())
	suspended := seedPublicOwner(ctx, t, store, suspendedID)
	seedExec(ctx, t, store, suspendedID, `UPDATE owners SET status = 'suspended' WHERE id = $1`, suspendedID)
	incompleteID := uuid.Must(uuid.NewV7())
	incomplete := seedBookingOwner(ctx, t, store, incompleteID) // no whatsapp, no address
	incomplete.host = "iso-" + incompleteID.String() + "." + testApex
	other := seedPublicOwner(ctx, t, store, uuid.Must(uuid.NewV7()))

	want := noTrace(h.do(tenantRequest(t, http.MethodGet, "/api/v1/public/owner", "nobody."+testApex, "")).Body.String())
	forged := tenantRequest(t, http.MethodGet, "/api/v1/public/owner", a.host, "")
	forged.Header.Del("X-Proxy-Secret")
	backofficeOnTenant := tenantRequest(t, http.MethodGet, "/api/v1/bookings", a.host, "")
	backofficeOnTenant.Header.Set("Authorization", "Bearer "+a.accessToken)
	for name, r := range map[string]*http.Request{
		"forged Host, no proxy secret": forged,
		"suspended owner":              tenantRequest(t, http.MethodGet, "/api/v1/public/owner", suspended.host, ""),
		"profile incomplete (BR-096)":  tenantRequest(t, http.MethodGet, "/api/v1/public/owner", incomplete.host, ""),
		"another owner's resource":     tenantRequest(t, http.MethodGet, "/api/v1/public/resources/"+other.resourceID, a.host, ""),
		"public route on backoffice":   tenantRequest(t, http.MethodGet, "/api/v1/public/owner", "example.com", ""),
		"backoffice route on tenant":   backofficeOnTenant,
	} {
		w := h.do(r)
		if w.Code != http.StatusNotFound || noTrace(w.Body.String()) != want {
			t.Errorf("%s: %d %s\nwant the same 404 as an unknown host: %s", name, w.Code, w.Body, want)
		}
	}
	// The portal needs only an active owner with a slug, not a live page.
	tok := testPortal.Token(incompleteID, uuid.MustParse(incomplete.bookingID))
	expect(t, h.do(tenantRequest(t, http.MethodGet, "/api/v1/portal/bookings/"+tok, incomplete.host, "")), http.StatusOK, "")
}

// S1-051 + BR-025: the catalogue and the request, never a unit code, a unit id
// or another renter; a draft that holds nothing; the neutral blacklist.
func TestPublicCatalogueAndRequest(t *testing.T) {
	ctx := t.Context()
	store := openAppStore(ctx, t)
	h := harness{t, newServer(t)}
	ownerID := uuid.Must(uuid.NewV7())
	s := seedPublicOwner(ctx, t, store, ownerID)
	unitCode := "ISO-" + ownerID.String()

	w := h.do(tenantRequest(t, http.MethodGet, "/api/v1/public/resources?"+future, s.host, ""))
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, s.marker) || !strings.Contains(body, `"available_count":1`) {
		t.Fatalf("catalogue = %d %s", w.Code, body)
	}
	for _, secret := range []string{unitCode, s.unitID, "iso-customer-", "owner_id"} {
		if strings.Contains(body, secret) {
			t.Errorf("catalogue leaks %q: %s", secret, body)
		}
	}
	// Signals the API-key lane uses are ignored on the tenant lane (BR-032).
	withKey := tenantRequest(t, http.MethodGet, "/api/v1/public/resources/"+s.resourceID+"?"+future, s.host, "")
	withKey.Header.Set("X-API-Key", "swn_live_notarealkeynotarealkeynotarealkey00")
	m := expect(t, h.do(withKey), http.StatusOK, "")
	if terms, _ := m["system_terms"].([]any); len(terms) < 3 || m["available"] != true {
		t.Errorf("detail = %v", m)
	}

	post := func(phone, start, end, extra string) *httptest.ResponseRecorder {
		r := tenantRequest(t, http.MethodPost, "/api/v1/public/bookings", s.host, fmt.Sprintf(
			`{"resource_id":"%s","start_at":"%s","end_at":"%s","customer":{"name":"Sari Publik","phone":"%s"}%s}`,
			s.resourceID, start, end, phone, extra))
		r.Header.Set("Idempotency-Key", uuid.NewString())
		return h.do(r)
	}
	m = expect(t, post("081277770000", "2030-03-01T09:00:00+07:00", "2030-03-03T09:00:00+07:00", ""), http.StatusCreated, "")
	if m["status"] != "draft" || !strings.HasPrefix(m["track_url"].(string), "http://"+s.host+"/booking/") {
		t.Errorf("created = %v", m)
	}
	var source string
	var expires, createdBy *any
	if err := store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT source, expires_at::text, created_by::text FROM bookings WHERE code = $1`, m["code"]).
			Scan(&source, &expires, &createdBy)
	}); err != nil {
		t.Fatal(err)
	}
	if source != "public_page" || expires == nil || createdBy != nil {
		t.Errorf("draft row: source=%s expires=%v created_by=%v", source, expires, createdBy)
	}
	// The same number in another spelling is the same renter.
	expect(t, post("+6281277770000", "2030-04-01T09:00:00+07:00", "2030-04-02T09:00:00+07:00", ""), http.StatusCreated, "")
	var renters int
	_ = store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM customers WHERE phone = '+6281277770000'`).Scan(&renters)
	})
	if renters != 1 {
		t.Errorf("%d customers for one phone, want 1", renters)
	}

	// Nothing free: 409, and no other renter's booking named.
	seedExec(ctx, t, store, ownerID, `INSERT INTO bookings (id, owner_id, code, customer_id, resource_id, resource_unit_id,
		start_at, end_at, status, unit_price, pricing_unit, duration_qty, subtotal)
		VALUES ($1, $2, 'HELD-1', $3, $4, $5, '2030-05-01 09:00+07', '2030-05-03 09:00+07', 'reserved', 1, 'day', 2, 2)`,
		uuid.Must(uuid.NewV7()), ownerID, s.customerID, s.resourceID, s.unitID)
	w = post("081200001111", "2030-05-02T09:00:00+07:00", "2030-05-04T09:00:00+07:00", "")
	expect(t, w, http.StatusConflict, "booking-conflict")
	if b := w.Body.String(); strings.Contains(b, "HELD-1") || strings.Contains(b, "conflicts") || strings.Contains(b, unitCode) {
		t.Errorf("conflict leaks: %s", b)
	}

	// BR-028 on the public page: neutral, no reason, no field.
	var renter string
	_ = store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id::text FROM customers WHERE phone = '+6281277770000'`).Scan(&renter)
	})
	expect(t, h.do(bodyRequest(t, http.MethodPost, "/api/v1/customers/"+renter+"/blacklist", s.accessToken,
		`{"reason":"merusak mobil"}`)), http.StatusOK, "")
	w = post("081277770000", "2030-06-01T09:00:00+07:00", "2030-06-02T09:00:00+07:00", "")
	expect(t, w, http.StatusUnprocessableEntity, "customer-blacklisted")
	if b := w.Body.String(); strings.Contains(b, "merusak") || strings.Contains(b, "customer_id") {
		t.Errorf("blacklist answer is not neutral: %s", b)
	}

	// A body naming the owner is an attempt, not a typo (04-api-spec.md §1).
	expect(t, post("081200002222", "2030-07-01T09:00:00+07:00", "2030-07-02T09:00:00+07:00",
		`,"owner_id":"`+uuid.NewString()+`"`), http.StatusUnprocessableEntity, "validation-failed")
	expect(t, post("081200002222", "2020-07-01T09:00:00+07:00", "2020-07-02T09:00:00+07:00", ""),
		http.StatusUnprocessableEntity, "validation-failed")
}

// §7: per IP, per owner, per portal token -- configuration, here made small.
func TestPublicRateLimits(t *testing.T) {
	ctx := t.Context()
	store := openAppStore(ctx, t)
	h := harness{t, newServerWith(t, config.PublicLimits{GetPerMinIP: 2, GetPerMinOwner: 3,
		PostPerHourIP: 1, PostPerHourOwner: 30, PortalPerMinToken: 1})}
	s := seedPublicOwner(ctx, t, store, uuid.Must(uuid.NewV7()))

	ip := "10.250." + fmt.Sprint(uuid.New()[0]) + ".1"
	get := func(fixedIP bool) int {
		r := tenantRequest(t, http.MethodGet, "/api/v1/public/owner", s.host, "")
		if fixedIP {
			r.Header.Set("X-Real-IP", ip)
		}
		return h.do(r).Code
	}
	if a, b, c := get(true), get(true), get(true); a != 200 || b != 200 || c != http.StatusTooManyRequests {
		t.Errorf("per IP: %d %d %d, want 200 200 429", a, b, c)
	}
	if a, b := get(false), get(false); a != 200 || b != http.StatusTooManyRequests {
		t.Errorf("per owner (3/min; the IP-refused one never reached the owner count): %d %d", a, b)
	}
	portal := "/api/v1/portal/bookings/" + s.portalToken
	if a, b := h.do(tenantRequest(t, http.MethodGet, portal, s.host, "")).Code,
		h.do(tenantRequest(t, http.MethodGet, portal, s.host, "")).Code; a != 200 || b != http.StatusTooManyRequests {
		t.Errorf("per token: %d %d", a, b)
	}
}

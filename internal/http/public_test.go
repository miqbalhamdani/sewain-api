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

// S1-053 + S1-062 + BR-002: one booking, its money and photos, a proof that
// reaches the backoffice -- and nothing else, from nowhere else.
func TestPortal(t *testing.T) {
	ctx := t.Context()
	store := openAppStore(ctx, t)
	h := harness{t, newServer(t)}
	ownerID := uuid.Must(uuid.NewV7())
	s := seedPortalOwner(ctx, t, store, ownerID)
	path := "/api/v1/portal/bookings/" + s.portalToken

	w := h.do(tenantRequest(t, http.MethodGet, path, s.host, ""))
	m := expect(t, w, http.StatusOK, "")
	invs, _ := m["invoices"].([]any)
	if m["code"] != "ISO-0001" || len(invs) != 1 || m["owner"].(map[string]any)["bank"] != nil {
		t.Errorf("portal = %v", m)
	}
	if strings.Contains(w.Body.String(), "ISO-"+ownerID.String()) {
		t.Errorf("portal shows the unit code (plate): %s", w.Body)
	}
	seedExec(ctx, t, store, ownerID, `UPDATE owners SET bank_name = 'BCA', bank_account_number = '1234567890',
		bank_account_holder = 'Budi' WHERE id = $1`, ownerID)
	m = expect(t, h.do(tenantRequest(t, http.MethodGet, path, s.host, "")), http.StatusOK, "")
	if bank, _ := m["owner"].(map[string]any)["bank"].(map[string]any); bank["account_number"] != "1234567890" {
		t.Errorf("bank = %v", m["owner"])
	}

	// Staff get the same link to send while WhatsApp is deferred (§5).
	if bk := expect(t, h.do(bearerRequest(t, http.MethodGet, "/api/v1/bookings/"+s.bookingID, s.accessToken)),
		http.StatusOK, ""); bk["portal_url"] != "http://"+s.host+"/booking/"+s.portalToken {
		t.Errorf("portal_url = %v", bk["portal_url"])
	}

	b := seedPortalOwner(ctx, t, store, uuid.Must(uuid.NewV7()))
	tampered := s.portalToken[:42] + map[bool]string{true: "A", false: "B"}[s.portalToken[42] != 'A']
	for name, r := range map[string]*http.Request{
		"tampered token":        tenantRequest(t, http.MethodGet, "/api/v1/portal/bookings/"+tampered, s.host, ""),
		"token on another host": tenantRequest(t, http.MethodGet, path, b.host, ""),
	} {
		expect(t, h.do(r), http.StatusNotFound, "not-found")
		_ = name
	}

	pres := expect(t, h.do(tenantRequest(t, http.MethodPost, path+"/uploads", s.host,
		`{"content_type":"image/png","bytes":8}`)), http.StatusCreated, "")
	key := pres["object_key"].(string)
	if !strings.HasPrefix(key, "pending/"+ownerID.String()+"/"+s.bookingID+"/") {
		t.Errorf("upload key %q is not scoped to the booking", key)
	}
	expect(t, h.do(tenantRequest(t, http.MethodPost, path+"/proofs", s.host,
		`{"invoice_id":"`+s.invoiceID+`","object_key":"pending/`+ownerID.String()+`/`+uuid.NewString()+`"}`)),
		http.StatusNotFound, "not-found")
	expect(t, h.do(tenantRequest(t, http.MethodPost, path+"/proofs", s.host,
		`{"invoice_id":"`+uuid.NewString()+`","object_key":"`+key+`"}`)), http.StatusNotFound, "not-found")

	put, _ := http.NewRequestWithContext(ctx, http.MethodPut, pres["upload_url"].(string), bytes.NewReader([]byte("PNGPROOF")))
	put.Header.Set("Content-Type", "image/png")
	if res, err := http.DefaultClient.Do(put); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("PUT to storage: %v %v", err, res)
	}
	expect(t, h.do(tenantRequest(t, http.MethodPost, path+"/proofs", s.host,
		`{"invoice_id":"`+s.invoiceID+`","object_key":"`+key+`"}`)), http.StatusAccepted, "")
	m = expect(t, h.do(tenantRequest(t, http.MethodGet, path, s.host, "")), http.StatusOK, "")
	if inv := m["invoices"].([]any)[0].(map[string]any); inv["proof_pending"] != true || inv["status"] != "unpaid" {
		t.Errorf("after upload: %v (a proof never pays anything by itself)", inv)
	}
	if w := h.do(bearerRequest(t, http.MethodGet, "/api/v1/invoices/"+s.invoiceID+"/proofs", s.accessToken)); w.Code != 200 ||
		!strings.Contains(w.Body.String(), `"review_status":"pending"`) {
		t.Errorf("backoffice does not see the renter's proof: %d %s", w.Code, w.Body)
	}
}

// S1-079 + S1-080 + BR-031/032: secret once, the four routes only, CORS as a
// browser control, quota per key, revocation immediate, lanes never crossed.
func TestAPIKeys(t *testing.T) {
	ctx := t.Context()
	store := openAppStore(ctx, t)
	h := harness{t, newServer(t)}
	ownerID := uuid.Must(uuid.NewV7())
	s := seedPublicOwner(ctx, t, store, ownerID)
	t.Cleanup(func() {
		c := context.WithoutCancel(ctx)
		_ = store.InOwnerTx(owner.NewContext(c, ownerID), func(tx pgx.Tx) error {
			_, err := tx.Exec(c, `DELETE FROM api_keys WHERE owner_id = $1`, ownerID)
			return err
		})
	})

	m := expect(t, h.do(bodyRequest(t, http.MethodPost, "/api/v1/api-keys", s.accessToken,
		`{"name":"situs uji","rate_limit_per_min":3}`)), http.StatusCreated, "")
	secret, keyID := m["secret"].(string), m["id"].(string)
	if !strings.HasPrefix(secret, "swn_live_") || len(secret) != 49 || m["key_prefix"] != secret[9:17] {
		t.Fatalf("created = %v", m)
	}
	if w := h.do(bearerRequest(t, http.MethodGet, "/api/v1/api-keys", s.accessToken)); strings.Contains(w.Body.String(), secret) {
		t.Errorf("list shows the secret: %s", w.Body)
	}

	ext := func(method, path, key, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Host = "api." + testApex
		r.Header.Set("X-Proxy-Secret", testProxySecret)
		r.Header.Set("X-Real-IP", "10.9.9.9")
		if key != "" {
			r.Header.Set("X-API-Key", key)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return h.do(r)
	}
	w := ext(http.MethodGet, "/api/v1/public/resources", secret, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), s.marker) {
		t.Fatalf("catalogue by key = %d %s", w.Code, w.Body)
	}
	expect(t, ext(http.MethodGet, "/api/v1/public/resources", "", ""), http.StatusUnauthorized, "invalid-api-key")
	w = ext(http.MethodGet, "/api/v1/public/resources", secret, "https://elsewhere.test")
	expect(t, w, http.StatusForbidden, "origin-not-allowed")
	if !strings.Contains(w.Header().Get("Vary"), "Origin") {
		t.Errorf("Vary = %q, want Origin", w.Header().Get("Vary"))
	}
	expect(t, h.do(bodyRequest(t, http.MethodPatch, "/api/v1/settings", s.accessToken,
		`{"allowed_origins":["https://rentalbudi.com/path"]}`)), http.StatusUnprocessableEntity, "validation-failed")
	expect(t, h.do(bodyRequest(t, http.MethodPatch, "/api/v1/settings", s.accessToken,
		`{"allowed_origins":["https://rentalbudi.test"]}`)), http.StatusOK, "")
	w = ext(http.MethodGet, "/api/v1/public/resources", secret, "https://rentalbudi.test")
	if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "https://rentalbudi.test" {
		t.Errorf("allowed origin: %d ACAO=%q", w.Code, w.Header().Get("Access-Control-Allow-Origin"))
	}
	w = ext(http.MethodOptions, "/api/v1/public/resources", "", "https://rentalbudi.test")
	if w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != "https://rentalbudi.test" {
		t.Errorf("preflight: %d %v", w.Code, w.Header())
	}

	// The four routes only; Host never names the tenant here, the key never does there.
	expect(t, ext(http.MethodGet, "/api/v1/bookings", secret, ""), http.StatusNotFound, "not-found")
	expect(t, ext(http.MethodGet, "/api/v1/portal/bookings/"+s.portalToken, secret, ""), http.StatusNotFound, "not-found")
	other := seedPublicOwner(ctx, t, store, uuid.Must(uuid.NewV7()))
	r := tenantRequest(t, http.MethodGet, "/api/v1/public/resources", other.host, "")
	r.Header.Set("X-API-Key", secret)
	if w := h.do(r); !strings.Contains(w.Body.String(), other.marker) || strings.Contains(w.Body.String(), s.marker) {
		t.Errorf("key changed the tenant on a tenant host: %s", w.Body)
	}

	// Quota 3/min: two used above, one more, then refused.
	ext(http.MethodGet, "/api/v1/public/owner", secret, "")
	expect(t, ext(http.MethodGet, "/api/v1/public/owner", secret, ""), http.StatusTooManyRequests, "quota-exceeded")

	if w := h.do(bearerRequest(t, http.MethodDelete, "/api/v1/api-keys/"+keyID, s.accessToken)); w.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", w.Code, w.Body)
	}
	expect(t, ext(http.MethodGet, "/api/v1/public/owner", secret, ""), http.StatusUnauthorized, "invalid-api-key")

	op := seedUserIn(ctx, t, store, ownerID, "operator")
	expect(t, h.do(bearerRequest(t, http.MethodGet, "/api/v1/api-keys", op.accessToken)), http.StatusForbidden, "permission-denied")
}

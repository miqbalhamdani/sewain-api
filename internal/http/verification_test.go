package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/owner"
)

// The acceptance for S1-082 and S1-084.
//
// Until register existed, login had no row to verify against and every
// dependency chain in this project bottomed out at "assume the owners row is
// already there". These are the tests that stop assuming.

// TestRegisterCreatesBusinessAndOwner is S1-082.
func TestRegisterCreatesBusinessAndOwner(t *testing.T) {
	ctx := t.Context()
	store := openAppStore(ctx, t)
	srv := newServer(t)

	// A distinct source address per call: the limit is 5/hour per IP, and a
	// shared one makes later subtests fail for the wrong reason. A counter,
	// not a uuid prefix -- uuid v7 leads with a timestamp, so the first bytes
	// of two ids minted in the same millisecond are identical.
	var ip int
	post := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		ip++
		r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = fmt.Sprintf("203.0.113.%d:1234", ip%250+1)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}
	freshEmail := func() string {
		return "reg-" + uuid.Must(uuid.NewV7()).String() + "@example.com"
	}
	body := func(email, businessType string) string {
		return `{"email":"` + email + `","password":"a password long enough",` +
			`"business_name":"Rental Coba","business_type":"` + businessType + `"}`
	}

	var ownerID string

	t.Run("201 carries a live session", func(t *testing.T) {
		w := post(t, body(freshEmail(), "vehicle_rental"))
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201\nbody: %s", w.Code, w.Body.String())
		}

		var got struct {
			AccessToken string `json:"access_token"`
			User        struct {
				Role            string  `json:"role"`
				EmailVerifiedAt *string `json:"email_verified_at"`
			} `json:"user"`
			Owner struct {
				ID   string  `json:"id"`
				Name string  `json:"name"`
				Slug *string `json:"slug"`
			} `json:"owner"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.AccessToken == "" {
			t.Error("no access token: the owner would have to log in again (BR-005)")
		}
		if got.User.Role != "owner" {
			t.Errorf("role = %q, want owner", got.User.Role)
		}
		// The session is live but the address is unproved -- that is the whole
		// shape of BR-006, and it is why they land on the wall not the
		// dashboard.
		if got.User.EmailVerifiedAt != nil {
			t.Errorf("email_verified_at = %v, want null on a fresh registration", *got.User.EmailVerifiedAt)
		}
		// slug is not asked for at registration (BR-005, BR-025).
		if got.Owner.Slug != nil {
			t.Errorf("slug = %q, want null: it is not asked for here", *got.Owner.Slug)
		}
		ownerID = got.Owner.ID

		// The refresh token is a host-only cookie: no Domain attribute, or it
		// would be sent to every other tenant's subdomain (BR-025).
		for _, c := range w.Result().Cookies() {
			if c.Name == "refresh_token" && c.Domain != "" {
				t.Errorf("refresh cookie has Domain=%q; it must be host-only", c.Domain)
			}
		}
	})

	t.Run("a taken address is 422 email-taken", func(t *testing.T) {
		email := freshEmail()
		if w := post(t, body(email, "vehicle_rental")); w.Code != http.StatusCreated {
			t.Fatalf("first register: %d\nbody: %s", w.Code, w.Body.String())
		}

		w := post(t, body(email, "vehicle_rental"))
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422\nbody: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "email-taken") {
			t.Errorf("type is not email-taken: %s", w.Body.String())
		}
	})

	// A business without an owner is a row nobody can ever log into, and it
	// would hold the address hostage against the retry.
	// A business without an owner is a row nobody can ever log into, and it
	// would hold the address hostage against the retry.
	//
	// Counted as a delta rather than an absolute: owners has no RLS, so an
	// absolute count sees every leftover any other test ever left behind, and
	// the assertion would be about the state of the database rather than about
	// this registration.
	t.Run("a rejected registration leaves no orphan business", func(t *testing.T) {
		ownerCtx := owner.NewContext(ctx, uuid.MustParse(ownerID))
		countOwners := func() int {
			t.Helper()
			var n int
			if err := store.InOwnerTx(ownerCtx, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM owners`).Scan(&n)
			}); err != nil {
				t.Fatalf("count owners: %v", err)
			}
			return n
		}

		email := freshEmail()
		if w := post(t, body(email, "vehicle_rental")); w.Code != http.StatusCreated {
			t.Fatalf("first register: %d\nbody: %s", w.Code, w.Body.String())
		}

		before := countOwners()
		if w := post(t, body(email, "vehicle_rental")); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("second register: %d, want 422\nbody: %s", w.Code, w.Body.String())
		}
		if after := countOwners(); after != before {
			t.Errorf("a rejected registration created %d business row(s) with no user", after-before)
		}
	})

	t.Run("a business_type outside the six presets is 422", func(t *testing.T) {
		if w := post(t, body(freshEmail(), "spaceship_rental")); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422\nbody: %s", w.Code, w.Body.String())
		}
	})

	// Six in one hour from one address: the sixth is refused (§7).
	t.Run("the sixth attempt from one IP is 429", func(t *testing.T) {
		ip := "198.51.100.7:5000"
		var last *httptest.ResponseRecorder
		for range 6 {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register",
				strings.NewReader(body(freshEmail(), "vehicle_rental")))
			r.Header.Set("Content-Type", "application/json")
			r.RemoteAddr = ip
			last = httptest.NewRecorder()
			srv.ServeHTTP(last, r)
		}
		if last.Code != http.StatusTooManyRequests {
			t.Errorf("sixth attempt = %d, want 429\nbody: %s", last.Code, last.Body.String())
		}
		if last.Header().Get("Retry-After") == "" {
			t.Error("429 carries no Retry-After")
		}
	})
}

// TestVerificationGate is S1-084: nothing outside the whitelist works until
// the address is proved, and everything in it does.
func TestVerificationGate(t *testing.T) {
	ctx := t.Context()
	store := openAppStore(ctx, t)
	srv := newServer(t)

	email := "gate-" + uuid.Must(uuid.NewV7()).String() + "@example.com"
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(
		`{"email":"`+email+`","password":"a password long enough",`+
			`"business_name":"Gate Rental","business_type":"equipment_rental"}`))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "192.0.2.44:9999"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("register: %d\nbody: %s", w.Code, w.Body.String())
	}

	var session struct {
		AccessToken string `json:"access_token"`
		User        struct {
			ID string `json:"id"`
		} `json:"user"`
		Owner struct {
			ID string `json:"id"`
		} `json:"owner"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode session: %v", err)
	}

	call := func(method, path string) *httptest.ResponseRecorder {
		req := bearerRequest(t, method, path, session.AccessToken)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// Enforced in one middleware, so this list is a sample of "everything",
	// not an enumeration of it.
	t.Run("the backoffice is closed before verifying", func(t *testing.T) {
		for _, tc := range []struct{ method, path string }{
			{http.MethodGet, "/api/v1/settings"},
			{http.MethodGet, "/api/v1/users"},
		} {
			rec := call(tc.method, tc.path)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s = %d, want 403\nbody: %s", tc.method, tc.path, rec.Code, rec.Body.String())
				continue
			}
			if !strings.Contains(rec.Body.String(), "email-not-verified") {
				t.Errorf("%s %s: type is not email-not-verified: %s", tc.method, tc.path, rec.Body.String())
			}
		}
	})

	// The four things BR-006 allows: the way in, the way out, and the way to
	// see which you are in.
	t.Run("the whitelist still works", func(t *testing.T) {
		if rec := call(http.MethodGet, "/api/v1/me"); rec.Code != http.StatusOK {
			t.Errorf("GET /me = %d, want 200 -- the frontend renders the wall from it", rec.Code)
		}
		if rec := call(http.MethodPost, "/api/v1/auth/verify-email/resend"); rec.Code != http.StatusNoContent {
			t.Errorf("resend = %d, want 204 -- it is the only way out of the wall", rec.Code)
		}
	})

	t.Run("an unknown token is 422, twice over", func(t *testing.T) {
		for range 2 {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/verify-email",
				strings.NewReader(`{"token":"made-up"}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422\nbody: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "verification-token-invalid") {
				t.Errorf("type is not verification-token-invalid: %s", rec.Body.String())
			}
		}
	})

	// Verifying in the database rather than through the emailed link: the mail
	// body is Mailpit's business, and a test that scrapes an inbox is a test
	// that fails when nobody started one.
	t.Run("verifying opens what was closed", func(t *testing.T) {
		ownerID := uuid.MustParse(session.Owner.ID)
		if err := store.InOwnerTx(owner.NewContext(ctx, ownerID), func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE users SET email_verified_at = now() WHERE id = $1`,
				session.User.ID)
			return err
		}); err != nil {
			t.Fatalf("verify: %v", err)
		}

		// The old token still says unverified -- the flag rides in the token,
		// so it takes effect on the next one. That is the documented tradeoff,
		// and the client answers it by calling /auth/refresh.
		if rec := call(http.MethodGet, "/api/v1/settings"); rec.Code != http.StatusForbidden {
			t.Errorf("the token minted before verifying = %d, want it to still say 403", rec.Code)
		}

		refreshed := refreshSession(ctx, t, srv, w.Result().Cookies())
		req := bearerRequest(t, http.MethodGet, "/api/v1/settings", refreshed)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("after refresh GET /settings = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
		}
	})
}

func refreshSession(_ context.Context, t *testing.T, srv http.Handler, cookies []*http.Cookie) string {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d\nbody: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode refresh: %v", err)
	}
	return got.AccessToken
}

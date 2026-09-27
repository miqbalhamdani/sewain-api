package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/owner"
)

// The acceptance for S1-008, S1-009 and S1-010 that no other test can reach.
//
// S1-008 asked for two things that had no route to happen on until now: a 403
// that names the permission it wanted, and a 404 -- not a 403 -- for a row
// belonging to another rental. Both are below.

// TestOperatorIsRefusedByPermission is S1-008's first half.
//
// The operator is refused at requirePermission, and the problem body names
// settings:write. That naming is the difference between "you cannot" and "ask
// your owner for settings:write", and BR-003 makes it an acceptance criterion
// rather than a nicety.
func TestOperatorIsRefusedByPermission(t *testing.T) {
	ctx := t.Context()
	store := openAppStore(ctx, t)
	srv := newServer(t)

	ownerID := uuid.Must(uuid.NewV7())
	// seedAuthUser leaves role = 'operator', which is exactly what this needs.
	operator := seedSignedInUser(ctx, t, store, ownerID)

	for _, tc := range []struct {
		name, method, path, body, permission string
	}{
		{"patch settings", http.MethodPatch, "/api/v1/settings", `{"booking_code_prefix":"ABC"}`, "settings:write"},
		{"list users", http.MethodGet, "/api/v1/users", "", "users:read"},
		{"invite user", http.MethodPost, "/api/v1/users", `{"email":"x@example.com","name":"X"}`, "users:write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+operator.accessToken)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, r)

			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403\nbody: %s", w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("content type = %q, want application/problem+json", ct)
			}

			var problem struct {
				Type    string `json:"type"`
				Detail  string `json:"detail"`
				TraceID string `json:"trace_id"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
				t.Fatalf("decode problem: %v", err)
			}
			if !strings.Contains(problem.Detail, tc.permission) {
				t.Errorf("detail %q does not name the permission %q", problem.Detail, tc.permission)
			}
			if !strings.HasSuffix(problem.Type, "/permission-denied") {
				t.Errorf("type = %q, want it to end in /permission-denied", problem.Type)
			}
			if problem.TraceID == "" {
				t.Error("403 carries no trace_id")
			}
		})
	}
}

// TestAnotherRentalsUserIs404 is S1-008's second half.
//
// Owner A aims an id-taking route at owner B's user. The answer is 404, not
// 403: a row that cannot be seen and a row that does not exist have to be
// indistinguishable, because the difference between them is a directory of
// every other rental's user ids (BR-001).
//
// Nothing in the handler checks ownership. RLS makes B's row invisible inside
// InOwnerTx, the query finds nothing, and the 404 falls out of that.
func TestAnotherRentalsUserIs404(t *testing.T) {
	ctx := t.Context()
	store := openAppStore(ctx, t)
	srv := newServer(t)

	a := seedSignedInOwner(ctx, t, store, uuid.Must(uuid.NewV7()))
	b := seedSignedInOwner(ctx, t, store, uuid.Must(uuid.NewV7()))

	// An id that exists in another rental, and an id that exists nowhere. The
	// two answers have to be the same answer -- any difference between them is
	// a probe that tells an attacker which user ids are real elsewhere.
	nobody := uuid.Must(uuid.NewV7()).String()

	for _, tc := range []struct{ name, method, body string }{
		{"patch", http.MethodPatch, `{"status":"disabled"}`},
		{"delete", http.MethodDelete, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ask := func(id string) (int, string) {
				r := httptest.NewRequest(tc.method, "/api/v1/users/"+id, strings.NewReader(tc.body))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+a.accessToken)
				w := httptest.NewRecorder()
				srv.ServeHTTP(w, r)
				return w.Code, w.Body.String()
			}

			gotCode, gotBody := ask(b.userID)
			if gotCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (403 would confirm the id exists)\nbody: %s",
					gotCode, gotBody)
			}

			wantCode, wantBody := ask(nobody)
			if gotCode != wantCode {
				t.Errorf("another rental's id gives %d, a nonexistent id gives %d", gotCode, wantCode)
			}
			// Everything but the per-request bits: trace id differs by design,
			// and `instance` echoes the path the caller already knows.
			if strip(gotBody, b.userID) != strip(wantBody, nobody) {
				t.Errorf("the two refusals are distinguishable:\n  other rental: %s\n  nonexistent:  %s",
					gotBody, wantBody)
			}
		})
	}

	// And the row is untouched: a refused DELETE that still disabled the
	// account would be the leak the 404 was hiding.
	var status string
	if err := store.InOwnerTx(owner.NewContext(ctx, uuid.MustParse(b.marker)), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, b.userID).Scan(&status)
	}); err != nil {
		t.Fatalf("read back owner B's user: %v", err)
	}
	if status != "active" {
		t.Errorf("owner B's user status = %q, want active -- the refused call still wrote", status)
	}
}

// TestSettingsRoundTrip is S1-009: the knobs are readable by their consumers,
// not merely stored, and the database is what refuses the values BR-057 says
// are wrong.
func TestSettingsRoundTrip(t *testing.T) {
	ctx := t.Context()
	store := openAppStore(ctx, t)
	srv := newServer(t)

	o := seedSignedInOwner(ctx, t, store, uuid.Must(uuid.NewV7()))

	patch := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+o.accessToken)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}

	t.Run("defaults come from the schema, not from a handler", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
		r.Header.Set("Authorization", "Bearer "+o.accessToken)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200\nbody: %s", w.Code, w.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for field, want := range map[string]any{
			"booking_code_prefix":     "SWN",
			"payment_due_hours":       float64(24),
			"no_show_tolerance_hours": float64(3),
			"draft_expiry_hours":      float64(24),
		} {
			if got[field] != want {
				t.Errorf("%s = %v, want %v", field, got[field], want)
			}
		}
	})

	t.Run("an absent key leaves its value alone", func(t *testing.T) {
		if w := patch(t, `{"booking_code_prefix":"RBD"}`); w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200\nbody: %s", w.Code, w.Body.String())
		}
		w := patch(t, `{"payment_due_hours":48}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200\nbody: %s", w.Code, w.Body.String())
		}
		var got map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if got["booking_code_prefix"] != "RBD" {
			t.Errorf("the second PATCH blanked a knob it never mentioned: prefix = %v",
				got["booking_code_prefix"])
		}
	})

	// The two halves of BR-057, and the difference is the point: a zero
	// payment deadline is past due on the instant it is issued, a zero no-show
	// tolerance is an owner freeing the unit the moment the booking starts.
	t.Run("payment_due_hours 0 is refused by the database", func(t *testing.T) {
		if w := patch(t, `{"payment_due_hours":0}`); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422\nbody: %s", w.Code, w.Body.String())
		}
	})

	t.Run("no_show_tolerance_hours 0 is accepted", func(t *testing.T) {
		if w := patch(t, `{"no_show_tolerance_hours":0}`); w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200\nbody: %s", w.Code, w.Body.String())
		}
	})

	t.Run("a reserved subdomain is refused by the database", func(t *testing.T) {
		w := patch(t, `{"slug":"api"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422\nbody: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "slug-invalid") {
			t.Errorf("type is not slug-invalid: %s", w.Body.String())
		}
	})

	t.Run("a bad booking code prefix is refused", func(t *testing.T) {
		if w := patch(t, `{"booking_code_prefix":"toolongandlower"}`); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422\nbody: %s", w.Code, w.Body.String())
		}
	})
}

// TestUsersLifecycle is S1-010: invite, change, disable -- and never delete.
func TestUsersLifecycle(t *testing.T) {
	ctx := t.Context()
	store := openAppStore(ctx, t)
	srv := newServer(t)

	o := seedSignedInOwner(ctx, t, store, uuid.Must(uuid.NewV7()))

	call := func(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+o.accessToken)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}

	email := "invitee-" + uuid.Must(uuid.NewV7()).String() + "@example.com"
	var invitedID string

	t.Run("invite creates an account with no password", func(t *testing.T) {
		w := call(t, http.MethodPost, "/api/v1/users",
			`{"email":"`+email+`","name":"Operator Baru"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201\nbody: %s", w.Code, w.Body.String())
		}
		var got struct {
			ID     string `json:"id"`
			Role   string `json:"role"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Role != "operator" {
			t.Errorf("role = %q, want operator (the contract's default)", got.Role)
		}
		if got.Status != "invited" {
			t.Errorf("status = %q, want invited", got.Status)
		}
		invitedID = got.ID
	})

	// Unique across the whole system, not per rental. That is what buys login
	// its missing business parameter (BR-004).
	t.Run("a taken address is 422 email-taken", func(t *testing.T) {
		w := call(t, http.MethodPost, "/api/v1/users", `{"email":"`+email+`","name":"Lagi"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422\nbody: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "email-taken") {
			t.Errorf("type is not email-taken: %s", w.Body.String())
		}
	})

	t.Run("disable does not delete the row", func(t *testing.T) {
		if w := call(t, http.MethodDelete, "/api/v1/users/"+invitedID, ""); w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204\nbody: %s", w.Code, w.Body.String())
		}

		// created_by elsewhere has to stay explicable, so the row outlives the
		// account. Checked in the database, not in the response: the response
		// of a DELETE that really deleted would look the same.
		var status string
		if err := store.InOwnerTx(owner.NewContext(ctx, uuid.MustParse(o.marker)), func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, invitedID).Scan(&status)
		}); err != nil {
			t.Fatalf("the row is gone, not disabled: %v", err)
		}
		if status != "disabled" {
			t.Errorf("status = %q, want disabled", status)
		}
	})
}

// strip removes the parts of a problem body that are allowed to differ between
// two requests: the id the caller supplied, and the trace id of that request.
func strip(body, id string) string {
	body = strings.ReplaceAll(body, id, "<id>")
	if i := strings.Index(body, `"trace_id"`); i >= 0 {
		if j := strings.Index(body[i:], ","); j >= 0 {
			body = body[:i] + body[i+j:]
		} else if j := strings.Index(body[i:], "}"); j >= 0 {
			body = body[:i] + body[i+j:]
		}
	}
	return body
}

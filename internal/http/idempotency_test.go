package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/miqbalhamdani/sewain-api/internal/owner"
	"github.com/miqbalhamdani/sewain-api/internal/queue"
)

// S1-012's acceptance, tested against the middleware rather than a route.
//
// None of the eight endpoints that will require Idempotency-Key exist yet --
// the first is POST /bookings in S1-026 -- so there is nothing to point a
// request at. Driving the middleware directly is not a workaround for that: it
// is the only way to cover the branches that a single endpoint would exercise
// one at a time anyway, and it keeps this suite green when S1-026 lands.

const idemTestPath = "/api/v1/test-idempotent"

// markIdempotent adds a synthetic route to the same table the real ones use,
// so the test exercises accessFor rather than a parallel switch that could
// disagree with it.
func markIdempotent(t *testing.T) {
	t.Helper()
	key := "POST " + idemTestPath
	routeAccessTable[key] = routeAccess{idempotent: true}
	t.Cleanup(func() { delete(routeAccessTable, key) })
}

// fakeStore is Redis in a map. What is under test is the middleware's
// sequencing -- claim, run, store, replay -- and none of that is Redis's.
type fakeStore struct {
	m      map[string]string
	broken bool
}

func newFakeStore() *fakeStore { return &fakeStore{m: map[string]string{}} }

func (s *fakeStore) SetNX(_ context.Context, key, value string, _ time.Duration) (bool, error) {
	if s.broken {
		return false, errors.New("redis is down")
	}
	if _, taken := s.m[key]; taken {
		return false, nil
	}
	s.m[key] = value
	return true, nil
}

func (s *fakeStore) Get(_ context.Context, key string) (string, error) {
	if s.broken {
		return "", errors.New("redis is down")
	}
	v, ok := s.m[key]
	if !ok {
		return "", queue.ErrNotFound
	}
	return v, nil
}

func (s *fakeStore) Set(_ context.Context, key, value string) error {
	if s.broken {
		return errors.New("redis is down")
	}
	s.m[key] = value
	return nil
}

func (s *fakeStore) Del(_ context.Context, key string) error {
	delete(s.m, key)
	return nil
}

// harness wires the middleware around a handler that counts its own runs.
type harness struct {
	handler http.Handler
	runs    *atomic.Int64
	store   *fakeStore
	ownerID uuid.UUID
}

func newHarness(t *testing.T, respond func(w http.ResponseWriter, runs int64)) *harness {
	t.Helper()
	markIdempotent(t)

	runs := &atomic.Int64{}
	store := newFakeStore()

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		respond(w, runs.Add(1))
	})

	return &harness{
		handler: Idempotency(store)(inner),
		runs:    runs,
		store:   store,
		ownerID: uuid.Must(uuid.NewV7()),
	}
}

func (h *harness) post(t *testing.T, key string, as ...uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()

	ownerID := h.ownerID
	if len(as) > 0 {
		ownerID = as[0]
	}

	r := httptest.NewRequest(http.MethodPost, idemTestPath, strings.NewReader(`{}`))
	if key != "" {
		r.Header.Set(idempotencyHeader, key)
	}
	r = r.WithContext(owner.NewContext(r.Context(), ownerID))

	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func okJSON(w http.ResponseWriter, runs int64) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"run":` + string(rune('0'+runs)) + `}`))
}

// TestIdempotencyReplaysTheFirstResponse is the core of BR-090: the same key
// runs once, and the second call gets the first answer back.
func TestIdempotencyReplaysTheFirstResponse(t *testing.T) {
	h := newHarness(t, okJSON)
	key := uuid.Must(uuid.NewV7()).String()

	first := h.post(t, key)
	if first.Code != http.StatusCreated {
		t.Fatalf("first call = %d, want 201\nbody: %s", first.Code, first.Body)
	}

	second := h.post(t, key)

	if got := h.runs.Load(); got != 1 {
		t.Errorf("handler ran %d times, want 1 -- the second call reached it", got)
	}
	if second.Code != first.Code {
		t.Errorf("replayed status = %d, want %d", second.Code, first.Code)
	}
	if second.Body.String() != first.Body.String() {
		t.Errorf("replayed body = %q, want %q", second.Body.String(), first.Body.String())
	}
	if ct := second.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("replayed Content-Type = %q, want the original's", ct)
	}
	if second.Header().Get("Idempotent-Replay") != "true" {
		t.Error("a replay does not say so; support cannot tell it from the original")
	}
}

// TestIdempotencyInFlightIsConflict covers the window where the first request
// has claimed the key and not finished.
func TestIdempotencyInFlightIsConflict(t *testing.T) {
	h := newHarness(t, okJSON)
	key := uuid.Must(uuid.NewV7()).String()

	// The claim without the completion: exactly the state the first request is
	// in while its handler runs.
	if _, err := h.store.SetNX(t.Context(), "idem:"+h.ownerID.String()+":"+key,
		inFlightMarker, idempotencyTTL); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	w := h.post(t, key)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409\nbody: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "request-in-flight") {
		t.Errorf("type is not request-in-flight: %s", w.Body)
	}
	if h.runs.Load() != 0 {
		t.Error("the handler ran while an identical request was still in flight")
	}
}

// TestIdempotencyScopesKeysPerOwner: the key is client-generated, so two
// rentals can pick the same one. Neither may see the other's answer.
func TestIdempotencyScopesKeysPerOwner(t *testing.T) {
	h := newHarness(t, okJSON)
	key := uuid.Must(uuid.NewV7()).String()

	h.post(t, key)
	h.post(t, key, uuid.Must(uuid.NewV7())) // a different rental, same key

	if got := h.runs.Load(); got != 2 {
		t.Errorf("handler ran %d times, want 2 -- one rental replayed another's response", got)
	}
}

// TestIdempotencyRequiresTheHeader: on a route that opts in, the header is
// mandatory. A client without one has no safe retry, which is the entire point.
func TestIdempotencyRequiresTheHeader(t *testing.T) {
	h := newHarness(t, okJSON)

	w := h.post(t, "")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422\nbody: %s", w.Code, w.Body)
	}
	if h.runs.Load() != 0 {
		t.Error("the handler ran without an Idempotency-Key on a route that requires one")
	}
}

// TestIdempotencySkipsRoutesThatDoNotOptIn: absence in the table means the
// middleware is not involved at all.
func TestIdempotencySkipsRoutesThatDoNotOptIn(t *testing.T) {
	runs := &atomic.Int64{}
	handler := Idempotency(newFakeStore())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		runs.Add(1)
		w.WriteHeader(http.StatusOK)
	}))

	// /api/v1/me is in the table but not marked idempotent.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK || runs.Load() != 1 {
		t.Errorf("status = %d, runs = %d: a route that does not opt in was gated anyway",
			w.Code, runs.Load())
	}
}

// TestIdempotencyDoesNotPinAServerError is the trap this design has to avoid.
//
// The contract tells clients to retry with the SAME key. If a 500 were stored,
// that instruction would replay the failure for 24 hours and the handler would
// never run again -- a transient fault turned permanent by the very mechanism
// meant to make retrying safe.
func TestIdempotencyDoesNotPinAServerError(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, runs int64) {
		if runs == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	key := uuid.Must(uuid.NewV7()).String()

	if first := h.post(t, key); first.Code != http.StatusInternalServerError {
		t.Fatalf("first call = %d, want 500", first.Code)
	}

	second := h.post(t, key)
	if second.Code != http.StatusCreated {
		t.Errorf("retry after a 500 = %d, want 201 -- the error was pinned to the key",
			second.Code)
	}
	if h.runs.Load() != 2 {
		t.Errorf("handler ran %d times, want 2", h.runs.Load())
	}
}

// TestIdempotencyStoresClientErrors: a 422 is a decision, not a fault. Asking
// the same question again does not change the answer, and re-running the
// handler to produce it is wasted work.
func TestIdempotencyStoresClientErrors(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ int64) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"type":"validation-failed"}`))
	})
	key := uuid.Must(uuid.NewV7()).String()

	h.post(t, key)
	second := h.post(t, key)

	if h.runs.Load() != 1 {
		t.Errorf("handler ran %d times, want 1", h.runs.Load())
	}
	if second.Code != http.StatusUnprocessableEntity {
		t.Errorf("replayed status = %d, want 422", second.Code)
	}
}

// TestIdempotencyFailsOpen: the database constraints are what prevent a
// duplicate booking (BR-022). This middleware only improves the answer, so a
// cache outage must not become an outage on the endpoints that earn money.
func TestIdempotencyFailsOpen(t *testing.T) {
	h := newHarness(t, okJSON)
	h.store.broken = true

	w := h.post(t, uuid.Must(uuid.NewV7()).String())
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 -- a Redis outage refused the write", w.Code)
	}
	if h.runs.Load() != 1 {
		t.Error("the handler did not run while the store was unavailable")
	}
}

// TestIdempotencyStoresTheResponseVerbatim guards the encoding: a replay that
// re-serialises would drift from the original the moment a field is added.
func TestIdempotencyStoresTheResponseVerbatim(t *testing.T) {
	h := newHarness(t, okJSON)
	key := uuid.Must(uuid.NewV7()).String()
	h.post(t, key)

	raw, err := h.store.Get(t.Context(), "idem:"+h.ownerID.String()+":"+key)
	if err != nil {
		t.Fatalf("nothing stored under the key: %v", err)
	}

	var stored storedResponse
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatalf("stored value is not readable: %v", err)
	}
	if stored.Status != http.StatusCreated {
		t.Errorf("stored status = %d, want 201", stored.Status)
	}
	if string(stored.Body) != `{"run":1}` {
		t.Errorf("stored body = %q, want the bytes the handler wrote", stored.Body)
	}
}

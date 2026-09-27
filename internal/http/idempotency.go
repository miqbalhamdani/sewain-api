package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/miqbalhamdani/sewain-api/internal/owner"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
	"github.com/miqbalhamdani/sewain-api/internal/queue"
)

// Idempotency-Key.  (S1-012, BR-090)
//
// One middleware, not a check per handler: the endpoints that need this are
// the ones that create bookings and take money, and a guard remembered at each
// of them is a guard missed at the eighth.
//
// What it is NOT for: preventing data corruption. The exclusion constraint and
// the unique indexes already do that, and they do it under concurrency in a way
// no cache can. What this fixes is the RESPONSE -- CLAUDE.md puts it plainly:
// a double submit answered with `booking-conflict` blames the operator for a
// network timeout that was not their fault.

const (
	idempotencyHeader = "Idempotency-Key"

	// 24 hours, counted from the first request rather than from when the
	// handler finished -- see queue.Set and its KEEPTTL note.
	idempotencyTTL = 24 * time.Hour

	// The claim marker. A key holding this is a request that started and has
	// not finished, which is what separates "still running" (409) from
	// "finished, here it is again" (replay).
	inFlightMarker = "in-flight"
)

// storedResponse is the first response, kept verbatim so a retry gets the same
// status and the same bytes rather than something reconstructed.
type storedResponse struct {
	Status int                 `json:"status"`
	Header map[string][]string `json:"header"`
	Body   []byte              `json:"body"`
}

// IdempotencyStore is the Redis surface this needs, declared in the consumer so
// internal/queue stays unaware of what a key means.
type IdempotencyStore interface {
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string) error
	Del(ctx context.Context, key string) error
}

// Idempotency replays the first response for a repeated key.
//
// It runs after Authenticate, because the key is scoped per owner: two rentals
// picking the same client-generated UUID must not see each other's answer, and
// a key that was not scoped would let one replay the other's response by
// guessing.
func Idempotency(store IdempotencyStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !accessFor(r).idempotent {
				next.ServeHTTP(w, r)
				return
			}

			presented := r.Header.Get(idempotencyHeader)
			if presented == "" {
				// Required, not optional, on the routes that opt in: the whole
				// point is that a retry is safe, and a client that omits the
				// key has no retry that is (04-api-spec.md §2.1).
				writeError(w, r, apperrors.ValidationFailed(
					"Idempotency-Key is required on this endpoint. Send one UUID per user "+
						"intent, and reuse it on every retry of that intent.").
					WithFields(apperrors.Field{Name: "Idempotency-Key"}))
				return
			}

			ownerID, ok := owner.FromContext(r.Context())
			if !ok {
				writeError(w, r, apperrors.Unauthenticated("A bearer token is required."))
				return
			}
			key := "idem:" + ownerID.String() + ":" + presented

			claimed, err := store.SetNX(r.Context(), key, inFlightMarker, idempotencyTTL)
			if err != nil {
				// Fail open, and deliberately. The database constraints are
				// what prevent a duplicate booking (BR-022); this middleware
				// only improves the answer. Refusing every write because a
				// cache is unreachable would turn a degraded dependency into
				// an outage on the endpoints that earn money.
				slog.WarnContext(r.Context(), "idempotency store unavailable, proceeding without it",
					"error", err)
				next.ServeHTTP(w, r)
				return
			}

			if !claimed {
				replay(w, r, store, key)
				return
			}

			// Buffer the response so it can be stored before it is sent. The
			// other order would let a client get a response the store never
			// learned about, and the retry would run the handler twice.
			rec := &recorder{header: http.Header{}, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			finish(r.Context(), store, key, rec)
			rec.flush(w)
		})
	}
}

// replay answers a key that is already taken: either the stored response, or a
// 409 because the first request is still running.
func replay(w http.ResponseWriter, r *http.Request, store IdempotencyStore, key string) {
	raw, err := store.Get(r.Context(), key)
	if errors.Is(err, queue.ErrNotFound) {
		// Claimed a moment ago and expired since, which needs 24 hours to
		// happen. Treat it as in-flight rather than running the handler: the
		// safe answer to "I cannot tell" is "wait".
		writeError(w, r, apperrors.RequestInFlight())
		return
	}
	if err != nil {
		slog.WarnContext(r.Context(), "idempotency read failed, answering in-flight", "error", err)
		writeError(w, r, apperrors.RequestInFlight())
		return
	}

	if raw == inFlightMarker {
		writeError(w, r, apperrors.RequestInFlight())
		return
	}

	var stored storedResponse
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		slog.ErrorContext(r.Context(), "stored idempotent response is unreadable", "error", err)
		writeError(w, r, apperrors.Internal(err))
		return
	}

	for name, values := range stored.Header {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	// So a caller can tell a replay from the original. Useful in a support
	// conversation, and harmless to anyone who ignores it.
	w.Header().Set("Idempotent-Replay", "true")
	w.WriteHeader(stored.Status)
	_, _ = w.Write(stored.Body)
}

// finish stores the response, or releases the claim so a retry can really retry.
func finish(ctx context.Context, store IdempotencyStore, key string, rec *recorder) {
	// 5xx is released rather than stored. Storing it would pin a server error
	// to this key for 24 hours: the client retries with the same key, as the
	// contract tells it to, and gets the same failure replayed forever with
	// the handler never running again. 4xx IS stored -- a rejected request is
	// a decision, and repeating the question does not change the answer.
	if rec.status >= http.StatusInternalServerError {
		if err := store.Del(ctx, key); err != nil {
			slog.WarnContext(ctx, "could not release idempotency key after a server error",
				"error", err)
		}
		return
	}

	encoded, err := json.Marshal(storedResponse{
		Status: rec.status,
		Header: rec.header,
		Body:   rec.body.Bytes(),
	})
	if err != nil {
		slog.ErrorContext(ctx, "could not encode idempotent response", "error", err)
		_ = store.Del(ctx, key)
		return
	}
	if err := store.Set(ctx, key, string(encoded)); err != nil {
		// The response still goes out. The cost is that a retry re-runs the
		// handler, which the database constraints are there to survive.
		slog.WarnContext(ctx, "could not store idempotent response", "error", err)
	}
}

// recorder buffers a response so it can be read before it is written.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func (rec *recorder) Header() http.Header { return rec.header }

func (rec *recorder) WriteHeader(status int) {
	if rec.wrote {
		return
	}
	rec.status = status
	rec.wrote = true
}

func (rec *recorder) Write(b []byte) (int, error) {
	rec.wrote = true
	return rec.body.Write(b)
}

func (rec *recorder) flush(w http.ResponseWriter) {
	for name, values := range rec.header {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(rec.status)
	_, _ = w.Write(rec.body.Bytes())
}

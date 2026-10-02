// Package jobs is the one job runner: Redis Streams, consumed by cmd/worker,
// fed by the API and by cmd/scheduler.  (S1-040, BR-091)
//
// Delivery is at-least-once, so every handler must be idempotent BY DESIGN --
// a job can run twice when a worker dies between doing the work and acking it.
// That is a different mechanism from BR-090's Idempotency-Key, and the two are
// not interchangeable.
//
// A failing job is retried with exponential backoff, then moved to the
// dead-letter stream. Never dropped silently, never retried forever. Job state
// lives in Redis, not in a table (CLAUDE.md) -- notifications are the one
// durable exception, and they are not this package's.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Job is one unit of work. OwnerID is the rental it runs for: the worker puts
// it in the context before calling the handler, so every query the handler
// makes goes through InOwnerTx exactly like a request does (BR-001).
type Job struct {
	ID      string // stream entry id, set when read
	Type    string
	OwnerID uuid.UUID
	Payload json.RawMessage
	Attempt int
}

// Names are the Redis keys one runner uses. Tests use their own, so a running
// worker on the developer's machine never eats a test's jobs.
type Names struct {
	Stream string // pending work
	Group  string // consumer group of cmd/worker
	Retry  string // sorted set: jobs waiting for their backoff, scored by due time
	Dead   string // dead-letter stream (BR-091; shown on the dashboard in S1-063)
}

// Default is what cmd/api, cmd/worker and cmd/scheduler share.
var Default = Names{Stream: "jobs", Group: "workers", Retry: "jobs:retry", Dead: "jobs:dead"}

// Queue is the producing side.
type Queue struct {
	rdb   *redis.Client
	names Names
}

func NewQueue(rdb *redis.Client, names Names) *Queue { return &Queue{rdb: rdb, names: names} }

// Enqueue adds a job. payload is marshalled to JSON.
func (q *Queue) Enqueue(ctx context.Context, typ string, ownerID uuid.UUID, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", typ, err)
	}
	return q.add(ctx, q.names.Stream, Job{Type: typ, OwnerID: ownerID, Payload: raw})
}

func (q *Queue) add(ctx context.Context, stream string, j Job, extra ...any) error {
	values := append([]any{
		"type", j.Type, "owner_id", j.OwnerID.String(),
		"payload", string(j.Payload), "attempt", strconv.Itoa(j.Attempt),
	}, extra...)
	if err := q.rdb.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: values}).Err(); err != nil {
		return fmt.Errorf("enqueue %s: %w", j.Type, err)
	}
	return nil
}

func jobOf(m redis.XMessage) (Job, error) {
	get := func(k string) string { s, _ := m.Values[k].(string); return s }
	owner, err := uuid.Parse(get("owner_id"))
	if err != nil {
		return Job{}, fmt.Errorf("job %s: bad owner_id: %w", m.ID, err)
	}
	attempt, _ := strconv.Atoi(get("attempt"))
	return Job{ID: m.ID, Type: get("type"), OwnerID: owner, Payload: json.RawMessage(get("payload")), Attempt: attempt}, nil
}

// encode carries a job into the retry sorted set; pumpRetry (Lua) decodes it.
func encode(j Job) string {
	b, _ := json.Marshal(struct {
		Type    string          `json:"type"`
		OwnerID uuid.UUID       `json:"owner_id"`
		Payload json.RawMessage `json:"payload"`
		Attempt int             `json:"attempt"`
		Nonce   string          `json:"nonce"` // two identical retries must stay two members
	}{j.Type, j.OwnerID, j.Payload, j.Attempt, uuid.NewString()})
	return string(b)
}

// ErrPermanent wraps a failure no retry can fix -- a payload that does not
// parse, a row that does not exist. It goes straight to the dead-letter stream.
var ErrPermanent = errors.New("permanent job failure")

// Backoff is the wait before attempt n+1: 2^n seconds, capped at ten minutes.
func Backoff(attempt int) time.Duration {
	d := time.Second << min(attempt, 10)
	return min(d, 10*time.Minute)
}

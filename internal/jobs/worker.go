package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/miqbalhamdani/sewain-api/internal/owner"
)

// Handler does one job. It runs with the job's owner in the context.
type Handler func(ctx context.Context, j Job) error

// Worker is the consuming side (cmd/worker).
type Worker struct {
	q        *Queue
	handlers map[string]Handler
	consumer string

	MaxAttempts int                     // then dead-letter
	Backoff     func(int) time.Duration // wait before the next attempt
	ClaimIdle   time.Duration           // a job held this long by a silent consumer is taken over
	Block       time.Duration           // XREADGROUP wait

	// OnDead runs once a job is dead-lettered -- e.g. marking a tracked job
	// failed, so a screen polling GET /jobs/{id} stops waiting.
	OnDead func(ctx context.Context, j Job, cause error)
}

func NewWorker(rdb *redis.Client, names Names, consumer string) *Worker {
	if consumer == "" {
		host, _ := os.Hostname()
		consumer = host + "-" + strconv.Itoa(os.Getpid())
	}
	return &Worker{q: NewQueue(rdb, names), handlers: map[string]Handler{}, consumer: consumer,
		MaxAttempts: 5, Backoff: Backoff, ClaimIdle: 5 * time.Minute, Block: 2 * time.Second}
}

// Handle registers the handler for a job type. Every handler must be
// idempotent: delivery is at-least-once.
func (w *Worker) Handle(typ string, h Handler) { w.handlers[typ] = h }

// Run consumes until ctx ends.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.ensureGroup(ctx); err != nil {
		return err
	}
	for ctx.Err() == nil {
		if err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "worker tick", "error", err)
			time.Sleep(time.Second)
		}
	}
	return nil
}

// Tick is one pass: move due retries back, take over abandoned jobs, then read
// new ones. Exported so tests drive the loop step by step.
func (w *Worker) Tick(ctx context.Context) error {
	if err := w.ensureGroup(ctx); err != nil {
		return err
	}
	if err := w.pumpRetries(ctx); err != nil {
		return err
	}
	claimed, _, err := w.q.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream: w.q.names.Stream, Group: w.q.names.Group, Consumer: w.consumer,
		MinIdle: w.ClaimIdle, Start: "0-0", Count: 10,
	}).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("xautoclaim: %w", err)
	}
	for _, m := range claimed {
		w.process(ctx, m)
	}
	streams, err := w.q.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: w.q.names.Group, Consumer: w.consumer, Streams: []string{w.q.names.Stream, ">"},
		Count: 10, Block: w.Block,
	}).Result()
	if errors.Is(err, redis.Nil) || ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("xreadgroup: %w", err)
	}
	for _, s := range streams {
		for _, m := range s.Messages {
			w.process(ctx, m)
		}
	}
	return nil
}

func (w *Worker) ensureGroup(ctx context.Context) error {
	err := w.q.rdb.XGroupCreateMkStream(ctx, w.q.names.Stream, w.q.names.Group, "0").Err()
	if err != nil && err.Error() != "BUSYGROUP Consumer Group name already exists" {
		return fmt.Errorf("create consumer group: %w", err)
	}
	return nil
}

// process runs one message to a terminal outcome -- done, scheduled for retry,
// or dead-lettered -- and only then acks it. A crash before the ack leaves it
// pending, and XAUTOCLAIM hands it to someone else: that is the at-least-once.
func (w *Worker) process(ctx context.Context, m redis.XMessage) {
	j, err := jobOf(m)
	if err == nil {
		err = w.run(ctx, j)
	} else {
		err = fmt.Errorf("%w: %w", ErrPermanent, err)
	}
	if err != nil {
		w.fail(ctx, j, m, err)
	}
	w.q.rdb.XAck(ctx, w.q.names.Stream, w.q.names.Group, m.ID)
	w.q.rdb.XDel(ctx, w.q.names.Stream, m.ID)
}

func (w *Worker) run(ctx context.Context, j Job) (err error) {
	h, ok := w.handlers[j.Type]
	if !ok {
		return fmt.Errorf("%w: no handler for %q", ErrPermanent, j.Type)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler %s panicked: %v", j.Type, r)
		}
	}()
	return h(owner.NewContext(ctx, j.OwnerID), j)
}

func (w *Worker) fail(ctx context.Context, j Job, m redis.XMessage, cause error) {
	next := j
	next.Attempt++
	if next.Attempt < w.MaxAttempts && !errors.Is(cause, ErrPermanent) {
		due := time.Now().Add(w.Backoff(j.Attempt))
		if err := w.q.rdb.ZAdd(ctx, w.q.names.Retry, redis.Z{Score: float64(due.UnixMilli()), Member: encode(next)}).Err(); err == nil {
			slog.WarnContext(ctx, "job failed, will retry", "type", j.Type, "attempt", next.Attempt, "error", cause)
			return
		}
	}
	slog.ErrorContext(ctx, "job dead-lettered", "type", j.Type, "attempt", next.Attempt, "error", cause)
	if w.OnDead != nil {
		w.OnDead(ctx, j, cause)
	}
	_ = w.q.add(ctx, w.q.names.Dead, Job{Type: j.Type, OwnerID: j.OwnerID, Payload: j.Payload, Attempt: next.Attempt},
		"error", cause.Error(), "source_id", m.ID, "failed_at", time.Now().UTC().Format(time.RFC3339))
}

// pumpRetry moves due jobs from the retry set back onto the stream, each one
// atomically -- two workers pumping at once cannot both re-add the same job.
var pumpRetry = redis.NewScript(`
local due = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, 50)
for _, member in ipairs(due) do
  if redis.call('ZREM', KEYS[1], member) == 1 then
    local j = cjson.decode(member)
    redis.call('XADD', KEYS[2], '*', 'type', j.type, 'owner_id', j.owner_id,
      'payload', cjson.encode(j.payload), 'attempt', tostring(j.attempt))
  end
end
return #due`)

func (w *Worker) pumpRetries(ctx context.Context) error {
	err := pumpRetry.Run(ctx, w.q.rdb, []string{w.q.names.Retry, w.q.names.Stream}, time.Now().UnixMilli()).Err()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("pump retries: %w", err)
	}
	return nil
}

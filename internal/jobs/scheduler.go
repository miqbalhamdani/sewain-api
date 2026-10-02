package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Schedule is one recurring piece of work. Run enqueues -- it never does the
// work itself; the worker does, with retry and dead-letter (BR-091).
type Schedule struct {
	Name  string
	Every time.Duration
	Run   func(ctx context.Context, q *Queue) error
}

// Scheduler is cmd/scheduler: only the lease holder fires, and each slot of
// each schedule fires at most once even if the lease changes hands inside it.
//
// Two guards on purpose. The lease keeps a second replica idle; the per-slot
// key is what actually makes a double run impossible -- a holder that stalls
// past its lease and wakes up after a takeover would otherwise fire the same
// slot again.
type Scheduler struct {
	q         *Queue
	schedules []Schedule
	id        string
	leaseKey  string
	LeaseTTL  time.Duration
}

func NewScheduler(rdb *redis.Client, names Names, schedules []Schedule) *Scheduler {
	return &Scheduler{q: NewQueue(rdb, names), schedules: schedules, id: uuid.NewString(),
		leaseKey: names.Stream + ":scheduler:lease", LeaseTTL: 15 * time.Second}
}

// renew extends the lease only if this instance still holds it.
var renew = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0`)

// Leader takes or keeps the lease.
func (s *Scheduler) Leader(ctx context.Context) (bool, error) {
	ttl := s.LeaseTTL.Milliseconds()
	ok, err := s.q.rdb.SetNX(ctx, s.leaseKey, s.id, s.LeaseTTL).Result()
	if err != nil || ok {
		return ok, err
	}
	n, err := renew.Run(ctx, s.q.rdb, []string{s.leaseKey}, s.id, ttl).Int()
	return n == 1, err
}

// Tick fires every schedule whose current slot has not fired yet. The caller
// (cmd/scheduler) calls it about once a second; at is the clock.
func (s *Scheduler) Tick(ctx context.Context, at time.Time) error {
	leader, err := s.Leader(ctx)
	if err != nil || !leader {
		return err
	}
	for _, sc := range s.schedules {
		slot := at.Truncate(sc.Every).Unix()
		key := s.q.names.Stream + ":scheduler:" + sc.Name + ":" + strconv.FormatInt(slot, 10)
		first, err := s.q.rdb.SetNX(ctx, key, s.id, 2*sc.Every+time.Minute).Result()
		if err != nil {
			return fmt.Errorf("slot %s: %w", key, err)
		}
		if !first {
			continue
		}
		if err := sc.Run(ctx, s.q); err != nil {
			// The slot is spent either way: the work it enqueues has its own
			// retries, and re-firing a half-run schedule is the double run
			// BR-091 forbids.
			slog.ErrorContext(ctx, "schedule failed", "schedule", sc.Name, "error", err)
		}
	}
	return nil
}

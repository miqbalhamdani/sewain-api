package jobs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/miqbalhamdani/sewain-api/internal/owner"
	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
	"github.com/miqbalhamdani/sewain-api/internal/queue"
)

// S1-040 against the host's real Redis. Every test gets its own stream names,
// so a worker running on the machine never touches them.
func setup(t *testing.T) (*redis.Client, Names) {
	t.Helper()
	c, err := queue.New(t.Context(), config.RedisURL())
	if err != nil {
		t.Fatalf("redis: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p := "test:" + uuid.NewString()
	n := Names{Stream: p, Group: "g", Retry: p + ":retry", Dead: p + ":dead"}
	t.Cleanup(func() {
		rdb := c.Raw()
		keys, _ := rdb.Keys(context.Background(), p+"*").Result()
		if len(keys) > 0 {
			rdb.Del(context.Background(), keys...)
		}
	})
	return c.Raw(), n
}

// A failing job is retried with backoff and then dead-lettered -- not lost,
// not retried forever (BR-091 obligation 2). The handler sees the owner.
func TestRetryThenDeadLetter(t *testing.T) {
	rdb, n := setup(t)
	ownerID := uuid.New()
	var calls atomic.Int32
	w := NewWorker(rdb, n, "c1")
	w.MaxAttempts, w.Block = 3, 50*time.Millisecond
	w.Backoff = func(int) time.Duration { return 0 }
	w.Handle("boom", func(ctx context.Context, j Job) error {
		if got, _ := owner.FromContext(ctx); got != ownerID {
			t.Errorf("handler owner = %v, want %v", got, ownerID)
		}
		calls.Add(1)
		return errors.New("downstream is down")
	})
	if err := NewQueue(rdb, n).Enqueue(t.Context(), "boom", ownerID, map[string]int{"x": 1}); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if err := w.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Errorf("handler ran %d times, want 3 (MaxAttempts)", calls.Load())
	}
	dead, _ := rdb.XRange(t.Context(), n.Dead, "-", "+").Result()
	if len(dead) != 1 || dead[0].Values["error"] != "downstream is down" || dead[0].Values["attempt"] != "3" {
		t.Fatalf("dead-letter = %v, want one entry with its last error", dead)
	}
	if left, _ := rdb.XLen(t.Context(), n.Stream).Result(); left != 0 {
		t.Errorf("%d jobs still on the stream", left)
	}
}

// A permanent failure skips the retries.
func TestPermanentFailureGoesStraightToDeadLetter(t *testing.T) {
	rdb, n := setup(t)
	var calls atomic.Int32
	w := NewWorker(rdb, n, "c1")
	w.Block = 50 * time.Millisecond
	w.Handle("bad", func(context.Context, Job) error { calls.Add(1); return ErrPermanent })
	_ = NewQueue(rdb, n).Enqueue(t.Context(), "bad", uuid.New(), nil)
	_ = NewQueue(rdb, n).Enqueue(t.Context(), "unknown-type", uuid.New(), nil)
	for range 3 {
		_ = w.Tick(t.Context())
	}
	if calls.Load() != 1 {
		t.Errorf("permanent failure retried: %d calls", calls.Load())
	}
	if d, _ := rdb.XLen(t.Context(), n.Dead).Result(); d != 2 {
		t.Errorf("dead-letter has %d, want 2 (permanent + unknown type)", d)
	}
}

// A worker that dies holding a job does not lose it: another consumer takes
// it over once it has been idle long enough (at-least-once).
func TestAbandonedJobIsReclaimed(t *testing.T) {
	rdb, n := setup(t)
	_ = NewQueue(rdb, n).Enqueue(t.Context(), "work", uuid.New(), nil)

	dead := NewWorker(rdb, n, "dies")
	_ = dead.ensureGroup(t.Context())
	// Read without processing: the consumer "crashes" with the job pending.
	if _, err := rdb.XReadGroup(t.Context(), &redis.XReadGroupArgs{Group: n.Group, Consumer: "dies",
		Streams: []string{n.Stream, ">"}, Count: 1}).Result(); err != nil {
		t.Fatal(err)
	}

	var ran atomic.Int32
	alive := NewWorker(rdb, n, "alive")
	alive.ClaimIdle, alive.Block = 10*time.Millisecond, 50*time.Millisecond
	alive.Handle("work", func(context.Context, Job) error { ran.Add(1); return nil })
	time.Sleep(20 * time.Millisecond)
	_ = alive.Tick(t.Context())
	if ran.Load() != 1 {
		t.Fatalf("abandoned job ran %d times, want 1", ran.Load())
	}
}

// Two scheduler replicas ticking the same instants fire each slot once
// (BR-091 obligation 3).
func TestSchedulerNeverRunsTwice(t *testing.T) {
	rdb, n := setup(t)
	var fired atomic.Int32
	sched := []Schedule{{Name: "test", Every: time.Minute, Run: func(context.Context, *Queue) error {
		fired.Add(1)
		return nil
	}}}
	a, b := NewScheduler(rdb, n, sched), NewScheduler(rdb, n, sched)

	base := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for i := range 3 { // three slots, both replicas ticking each one concurrently
		at := base.Add(time.Duration(i) * time.Minute)
		for _, s := range []*Scheduler{a, b} {
			wg.Add(1)
			go func() { defer wg.Done(); _ = s.Tick(t.Context(), at) }()
		}
		wg.Wait()
	}
	if fired.Load() != 3 {
		t.Fatalf("fired %d times over 3 slots with 2 replicas, want 3", fired.Load())
	}

	// A takeover mid-slot does not re-fire it.
	rdb.Del(t.Context(), n.Stream+":scheduler:lease")
	_ = b.Tick(t.Context(), base.Add(2*time.Minute+30*time.Second))
	if fired.Load() != 3 {
		t.Errorf("new lease holder re-fired a spent slot: %d", fired.Load())
	}
}

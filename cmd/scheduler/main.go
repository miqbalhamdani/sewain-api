// Command scheduler puts scheduled work on the job stream.  (S1-040, BR-091)
//
// It never does the work: it enqueues, and cmd/worker runs it with retries and
// dead-letter. Only the Redis lease holder fires, and each slot fires once, so
// two replicas or a rolling deploy never double-send. This loop is the one
// sanctioned timer in the system -- never a time.Ticker in cmd/api.
//
// WhatsApp reminders (S1-054) register here when they land.
//
// `scheduler -once` fires every schedule once, right now, and exits -- "run the
// expiry sweep now" for an operator, and how the end-to-end suite expires a
// draft without waiting a day (S1-071). It skips the lease and the slot on
// purpose: it is a deliberate extra run, and every job is idempotent (BR-091).
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/miqbalhamdani/sewain-api/internal/booking"
	"github.com/miqbalhamdani/sewain-api/internal/jobs"
	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
	"github.com/miqbalhamdani/sewain-api/internal/queue"
)

var schedules = []jobs.Schedule{
	// Draft, payment-due and no-show expiry (S1-052). Not owner-scoped: the
	// worker walks every active rental.
	{Name: "expiry", Every: 5 * time.Minute, Run: func(ctx context.Context, q *jobs.Queue) error {
		return q.Enqueue(ctx, booking.JobExpirySweep, uuid.Nil, struct{}{})
	}},
}

func main() {
	once := flag.Bool("once", false, "fire every schedule once and exit")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	redis, err := queue.New(ctx, config.RedisURL())
	if err != nil {
		slog.Error("scheduler exited", "error", err)
		os.Exit(1)
	}
	defer func() { _ = redis.Close() }()

	if *once {
		q := jobs.NewQueue(redis.Raw(), jobs.Default)
		for _, sc := range schedules {
			if err := sc.Run(ctx, q); err != nil {
				slog.Error("scheduler -once", "schedule", sc.Name, "error", err)
				os.Exit(1)
			}
			slog.Info("fired", "schedule", sc.Name)
		}
		return
	}

	s := jobs.NewScheduler(redis.Raw(), jobs.Default, schedules)
	slog.Info("scheduler running", "schedules", len(schedules))
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			if err := s.Tick(ctx, now); err != nil {
				slog.Error("scheduler tick", "error", err)
			}
		}
	}
}

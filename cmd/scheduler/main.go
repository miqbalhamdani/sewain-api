// Command scheduler puts scheduled work on the job stream.  (S1-040, BR-091)
//
// It never does the work: it enqueues, and cmd/worker runs it with retries and
// dead-letter. Only the Redis lease holder fires, and each slot fires once, so
// two replicas or a rolling deploy never double-send. This loop is the one
// sanctioned timer in the system -- never a time.Ticker in cmd/api.
//
// The schedule list is empty in M4. Draft and payment expiry (S1-052) and the
// WhatsApp reminders (S1-054) register here.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/miqbalhamdani/sewain-api/internal/jobs"
	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
	"github.com/miqbalhamdani/sewain-api/internal/queue"
)

var schedules = []jobs.Schedule{}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	redis, err := queue.New(ctx, config.RedisURL())
	if err != nil {
		slog.Error("scheduler exited", "error", err)
		os.Exit(1)
	}
	defer func() { _ = redis.Close() }()

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

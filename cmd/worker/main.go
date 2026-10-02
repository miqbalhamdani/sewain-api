// Command worker consumes the job stream.  (S1-040, BR-091)
//
// Separate from cmd/scheduler on purpose (CLAUDE.md): this one does the heavy,
// externally-dependent work -- reading proofs today, exports and WhatsApp
// later -- and scales on its own. Every handler is idempotent: delivery is
// at-least-once.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/uuid"

	"github.com/miqbalhamdani/sewain-api/internal/booking"
	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/jobs"
	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
	"github.com/miqbalhamdani/sewain-api/internal/queue"
	"github.com/miqbalhamdani/sewain-api/internal/storage"
)

func main() {
	if err := run(); err != nil {
		slog.Error("worker exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.New(ctx, config.AppDatabaseURL())
	if err != nil {
		return err
	}
	defer pool.Close()
	redis, err := queue.New(ctx, config.RedisURL())
	if err != nil {
		return err
	}
	defer func() { _ = redis.Close() }()
	store, err := storage.FromConfig()
	if err != nil {
		return err
	}

	// Phase 1 has no proof reader: NoScanner leaves every proof "not read,
	// check by hand" (BR-062). A real one is a Scanner, swapped in here.
	bookings := booking.New(pool, store).WithJobs(nil, booking.NoScanner{})

	w := jobs.NewWorker(redis.Raw(), jobs.Default, "")
	w.Handle(booking.JobScanProof, func(ctx context.Context, j jobs.Job) error {
		var p struct {
			ProofID uuid.UUID `json:"proof_id"`
		}
		if err := json.Unmarshal(j.Payload, &p); err != nil || p.ProofID == uuid.Nil {
			return fmt.Errorf("%w: proof.scan payload %s", jobs.ErrPermanent, j.Payload)
		}
		return bookings.ScanProof(ctx, p.ProofID)
	})

	slog.Info("worker consuming", "stream", jobs.Default.Stream)
	return w.Run(ctx)
}

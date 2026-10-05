package booking

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
)

// Expiry.  (S1-052, BR-027, BR-038, BR-057)
//
// Three transitions and one invoice status, for one owner, in one
// transaction. Run by the worker's expiry.sweep job, which the scheduler
// enqueues -- never a timer in cmd/api (BR-091).

// Expired counts what one run changed, for the log.
// JobExpirySweep is the scheduled worker job that runs Expire for every
// active rental.
const JobExpirySweep = "expiry.sweep"

type Expired struct {
	Drafts, PaymentExpired, NoShows, OverdueInvoices int64
}

// Expire applies every due transition as of now. Each step is a conditional
// update, so running it twice changes nothing the second time -- the job is
// at-least-once (BR-091).
//
// The order matters. Payment expiry runs before no-show: with the switch on,
// due_at <= start_at, so a booking past its no-show point is always past its
// due date too, and BR-057 cancels it for non-payment rather than marking it
// a no-show it never had the chance to be.
func (s *Service) Expire(ctx context.Context, now time.Time) (Expired, error) {
	var out Expired
	err := s.store.InOwnerTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		drafts, err := q.ExpireDrafts(ctx, now)
		if err != nil {
			return fmt.Errorf("expire drafts: %w", err)
		}
		out.Drafts = int64(len(drafts))

		unpaid, err := q.ExpireUnpaidReserved(ctx, now)
		if err != nil {
			return fmt.Errorf("expire unpaid: %w", err)
		}
		out.PaymentExpired = int64(len(unpaid))
		if len(unpaid) > 0 {
			if err := q.CancelInvoicesOfBookings(ctx, unpaid); err != nil {
				return fmt.Errorf("cancel invoices of expired bookings: %w", err)
			}
		}

		if out.OverdueInvoices, err = q.MarkInvoicesOverdue(ctx, now); err != nil {
			return fmt.Errorf("mark invoices overdue: %w", err)
		}
		if out.NoShows, err = q.MarkNoShows(ctx, now); err != nil {
			return fmt.Errorf("mark no-shows: %w", err)
		}
		return nil
	})
	return out, err
}

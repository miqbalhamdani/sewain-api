package booking

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/miqbalhamdani/sewain-api/internal/db/sqlcgen"
	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// S1-023: the test that proves the product's main claim, and that lives in
// `make check` forever (`make test-race` runs it under -race).
//
// Two tests, because each alone has a hole:
//
//   - TestConcurrentCreate is the acceptance as written -- two operators press
//     save at the same moment, exactly one wins, the other reads
//     booking-conflict. But the pre-check in Create can catch the loser on
//     its own when the scheduler serialises the goroutines, so it would stay
//     green on some runs even with the constraint gone.
//   - TestConstraintRefusesConcurrentInsert closes that: it writes the second
//     row while the first is still uncommitted, past any pre-check, and only
//     bookings_no_overlap can refuse it. Drop the constraint, or "optimise" it
//     into an application check, and this goes red every time.
func TestConcurrentCreate(t *testing.T) {
	f := newFixture(t, 1)
	s := New(f.store)

	for round := range 10 {
		start := time.Date(2027, 1, 1, 9, 0, 0, 0, time.UTC).AddDate(0, 0, round*3)
		n := NewBooking{CustomerID: f.customer, UnitID: f.units[0], StartAt: start, EndAt: start.Add(48 * time.Hour)}

		var wg sync.WaitGroup
		gate := make(chan struct{})
		errs := make([]error, 2)
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				_, errs[i] = s.Create(f.ctx, f.userID, n)
			}()
		}
		close(gate)
		wg.Wait()

		wins, conflicts := 0, 0
		for _, err := range errs {
			var known *apperrors.Error
			switch {
			case err == nil:
				wins++
			case errors.As(err, &known) && known.Code == apperrors.CodeBookingConflict:
				conflicts++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if wins != 1 || conflicts != 1 {
			t.Fatalf("round %d: %d wins, %d conflicts; want exactly 1 and 1", round, wins, conflicts)
		}
	}
}

func TestConstraintRefusesConcurrentInsert(t *testing.T) {
	f := newFixture(t, 1)
	start := time.Date(2027, 6, 1, 9, 0, 0, 0, time.UTC)

	insert := func(tx pgx.Tx, code string) error {
		return sqlcgen.New(tx).InsertBooking(f.ctx, sqlcgen.InsertBookingParams{
			ID: uuid.Must(uuid.NewV7()), OwnerID: f.ownerID, Code: code,
			CustomerID: f.customer, ResourceID: f.resource, ResourceUnitID: f.units[0],
			StartAt: start, EndAt: start.Add(48 * time.Hour), Status: "reserved", Source: "staff",
			UnitPrice: 1, PricingUnit: "day", DurationQty: 2, Subtotal: 2,
		})
	}

	inserted, release := make(chan struct{}), make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- f.store.InOwnerTx(f.ctx, func(tx pgx.Tx) error {
			if err := insert(tx, "RACE-1"); err != nil {
				close(inserted)
				return err
			}
			close(inserted)
			<-release // hold the row uncommitted while the second insert arrives
			return nil
		})
	}()

	<-inserted
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- f.store.InOwnerTx(f.ctx, func(tx pgx.Tx) error { return insert(tx, "RACE-2") })
	}()
	close(release)

	if err := <-firstDone; err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := <-secondDone
	if err == nil {
		t.Fatal("two overlapping reserved bookings were both written -- bookings_no_overlap is gone (BR-022)")
	}
	if !errors.Is(translate(err, "insert"), errOverlap) {
		t.Fatalf("second insert failed with %v, want the bookings_no_overlap refusal", err)
	}
}

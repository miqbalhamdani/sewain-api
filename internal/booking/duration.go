package booking

import (
	"fmt"
	"strconv"
	"time"

	apperrors "github.com/miqbalhamdani/sewain-api/internal/platform/errors"
)

// unitLength is one pricing unit as a duration. A month is 30 days, fixed:
// calendar months would make February cheaper per day than March, and two
// renters with the same duration would pay differently (04-api-spec.md 3.3).
var unitLength = map[string]time.Duration{
	"hour":  time.Hour,
	"day":   24 * time.Hour,
	"week":  7 * 24 * time.Hour,
	"month": 30 * 24 * time.Hour,
}

var unitName = map[string]string{"hour": "jam", "day": "hari", "week": "minggu", "month": "bulan"}

// durationQty is ceil(duration / pricing unit): 25 hours on a daily price is
// two days. Started units are charged whole, the same rounding BR-046 uses for
// late fees.
func durationQty(start, end time.Time, pricingUnit string) (int32, error) {
	length, ok := unitLength[pricingUnit]
	if !ok {
		// pricing_unit is CHECKed to these four; reaching here is a bug.
		return 0, fmt.Errorf("unknown pricing unit %q", pricingUnit)
	}
	d := end.Sub(start)
	qty := d / length
	if d%length != 0 {
		qty++
	}
	return int32(qty), nil //nolint:gosec // a booking is not 2^31 units long
}

// checkDuration applies BR-021: each bound only when set, independently. A nil
// bound is no bound, never zero (BR-016).
func checkDuration(qty int32, minDur, maxDur *int32, pricingUnit string) error {
	unit := unitName[pricingUnit]
	if minDur != nil && qty < *minDur {
		return apperrors.DurationOutOfRange(
			"This resource is rented for at least " + strconv.Itoa(int(*minDur)) + " " + unit + ".")
	}
	if maxDur != nil && qty > *maxDur {
		return apperrors.DurationOutOfRange(
			"This resource is rented for at most " + strconv.Itoa(int(*maxDur)) + " " + unit + ".")
	}
	return nil
}

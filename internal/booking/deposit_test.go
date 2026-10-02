package booking

import (
	"testing"

	"github.com/google/uuid"
)

// BR-048 as arithmetic: deducted + refunded is always the deposit, nothing is
// ever negative, and the shortfall keeps each damage line's photo.
func TestAbsorb(t *testing.T) {
	photo := uuid.New()
	charges := []charge{
		{kind: "late_fee", description: "Telat 2 hari", amount: 200000},
		{kind: "damage", description: "Baret pintu", amount: 500000, photoID: &photo},
	}

	d, r, rest := absorb(1000000, charges)
	if d != 700000 || r != 300000 || len(rest) != 0 {
		t.Errorf("deposit covers all: %d / %d / %v", d, r, rest)
	}

	d, r, rest = absorb(500000, charges)
	if d != 500000 || r != 0 || len(rest) != 1 || rest[0].amount != 200000 ||
		rest[0].kind != "damage" || rest[0].photoID != &photo {
		t.Errorf("shortfall: %d / %d / %+v", d, r, rest)
	}

	d, r, rest = absorb(500000, nil)
	if d != 0 || r != 500000 || rest != nil {
		t.Errorf("no charges: full refund, got %d / %d / %v", d, r, rest)
	}
}

// Package public holds the decisions the public surface makes on its own.
// (S1-051, BR-095)
//
// Everything else on that surface is a read of the catalogue and the booking
// services, trimmed where it becomes the contract type in internal/http.
package public

import (
	"fmt"
	"strconv"
)

// Knobs are the owner settings the system sentences are assembled from.
type Knobs struct {
	PaymentDueHours            int
	NoShowToleranceHours       int
	RequirePaymentBeforePickup bool
}

var unitSentence = map[string]string{
	"hour":  "Dihitung per jam",
	"day":   "1 hari = 24 jam",
	"week":  "1 minggu = 7 × 24 jam",
	"month": "1 bulan = 30 hari",
}

var unitWord = map[string]string{"hour": "jam", "day": "hari", "week": "minggu", "month": "bulan"}

// SystemTerms are BR-095's four sentences: built from the owner's knobs and the
// resource's own price rules, never from text the owner typed, and never
// editable. A rule that does not apply (no late fee) is left out rather than
// rendered as "Rp 0".
func SystemTerms(pricingUnit string, lateFeePerUnit *int64, k Knobs) []string {
	out := []string{}
	if s, ok := unitSentence[pricingUnit]; ok {
		out = append(out, s)
	}
	pay := fmt.Sprintf("Bayar paling lambat %d jam setelah booking dikonfirmasi", k.PaymentDueHours)
	if k.RequirePaymentBeforePickup {
		pay += ", atau booking batal otomatis"
	}
	out = append(out, pay)
	if lateFeePerUnit != nil {
		out = append(out, fmt.Sprintf("Telat kembali dikenakan %s per %s", Rupiah(*lateFeePerUnit), unitWord[pricingUnit]))
	}
	if k.NoShowToleranceHours == 0 {
		out = append(out, "Tidak datang saat jadwal mulai = batal")
	} else {
		out = append(out, fmt.Sprintf("Tidak datang lewat %d jam dari jadwal = batal", k.NoShowToleranceHours))
	}
	return out
}

// Rupiah formats whole rupiah the Indonesian way: Rp 1.250.000.
func Rupiah(v int64) string {
	s := strconv.FormatInt(v, 10)
	neg := s[0] == '-'
	if neg {
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "." + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return "Rp " + s
}

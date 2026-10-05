package public

import (
	"reflect"
	"testing"
)

func TestSystemTerms(t *testing.T) {
	fee := int64(100000)
	got := SystemTerms("day", &fee, Knobs{PaymentDueHours: 24, NoShowToleranceHours: 3, RequirePaymentBeforePickup: true})
	want := []string{
		"1 hari = 24 jam",
		"Bayar paling lambat 24 jam setelah booking dikonfirmasi, atau booking batal otomatis",
		"Telat kembali dikenakan Rp 100.000 per hari",
		"Tidak datang lewat 3 jam dari jadwal = batal",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("terms =\n%q\nwant\n%q", got, want)
	}
	// No late fee, switch off, zero tolerance: the rule that does not apply is absent.
	got = SystemTerms("day", nil, Knobs{PaymentDueHours: 6})
	if len(got) != 3 || got[1] != "Bayar paling lambat 6 jam setelah booking dikonfirmasi" ||
		got[2] != "Tidak datang saat jadwal mulai = batal" {
		t.Errorf("terms without fee = %q", got)
	}
	for in, want := range map[int64]string{0: "Rp 0", 999: "Rp 999", 1000: "Rp 1.000", 1250000: "Rp 1.250.000"} {
		if got := Rupiah(in); got != want {
			t.Errorf("Rupiah(%d) = %q, want %q", in, got, want)
		}
	}
}

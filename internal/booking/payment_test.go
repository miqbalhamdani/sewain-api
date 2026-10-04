package booking

import "testing"

// Aturan 1-5 ide booking-invoice-lists, sebagai fungsi murni.
func TestPaymentOf(t *testing.T) {
	for _, tc := range []struct {
		name, status            string
		active, overdue, unpaid int32
		outstanding             int64
		wantStatus              string
		wantOutstanding         int64
	}{
		{"draft selalu none", "draft", 1, 0, 1, 500, "none", 0},
		{"cancelled selalu none", "cancelled", 2, 1, 1, 500, "none", 0},
		{"no_show selalu none", "no_show", 1, 0, 1, 500, "none", 0},
		{"tanpa invoice aktif", "reserved", 0, 0, 0, 0, "none", 0},
		{"overdue menang atas unpaid", "picked_up", 2, 1, 1, 750, "overdue", 750},
		{"unpaid", "reserved", 2, 0, 1, 500, "unpaid", 500},
		{"semua lunas", "completed", 2, 0, 0, 0, "paid", 0},
	} {
		s, o := paymentOf(tc.status, tc.active, tc.overdue, tc.unpaid, tc.outstanding)
		if s != tc.wantStatus || o != tc.wantOutstanding {
			t.Errorf("%s: paymentOf = %s/%d, want %s/%d", tc.name, s, o, tc.wantStatus, tc.wantOutstanding)
		}
	}
}

package booking

import (
	"testing"
	"time"
)

func TestSegments(t *testing.T) {
	at := func(day, hour int) time.Time { return time.Date(2026, 9, day, hour, 0, 0, 0, time.UTC) }
	from, to := at(1, 0), at(10, 0)

	t.Run("maintenance is one segment over the whole range", func(t *testing.T) {
		got := segments(from, to, "maintenance", []calBooking{{StartAt: at(3, 9), EndAt: at(5, 9),
			EndAtWithBuffer: at(5, 9), Status: "reserved"}}, at(4, 0))
		if len(got) != 1 || got[0].State != StateMaintenance || !got[0].From.Equal(from) || !got[0].To.Equal(to) {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("reserved, buffer, picked up, overdue, gapless", func(t *testing.T) {
		got := segments(from, to, "active", []calBooking{
			{StartAt: at(2, 9), EndAt: at(4, 10), EndAtWithBuffer: at(4, 12), Status: "reserved"},
			{StartAt: at(4, 12), EndAt: at(6, 9), EndAtWithBuffer: at(6, 9), Status: "picked_up"},
			{StartAt: at(8, 0), EndAt: at(12, 0), EndAtWithBuffer: at(12, 0), Status: "picked_up"},
		}, at(7, 0))
		want := []string{StateAvailable, StateReservedUnpaid, StateBuffer, StateOverdue,
			StateAvailable, StatePickedUp}
		if len(got) != len(want) {
			t.Fatalf("got %d segments %+v, want %v", len(got), got, want)
		}
		for i, s := range got {
			if s.State != want[i] {
				t.Errorf("segment %d = %s, want %s", i, s.State, want[i])
			}
			if i > 0 && !got[i-1].To.Equal(s.From) {
				t.Errorf("gap between %d and %d", i-1, i)
			}
		}
		if !got[0].From.Equal(from) || !got[len(got)-1].To.Equal(to) {
			t.Error("segments do not cover [from, to) -- the last booking must be clipped at to")
		}
		if got[1].Booking == nil || got[2].Booking != nil {
			t.Error("booking ref belongs on the booking segment, not the buffer")
		}
	})

	t.Run("a booking that started before the window is clipped, not dropped", func(t *testing.T) {
		got := segments(from, to, "active", []calBooking{
			{StartAt: at(1, 0).Add(-48 * time.Hour), EndAt: at(2, 0), EndAtWithBuffer: at(2, 0), Status: "reserved"},
		}, at(1, 0))
		if got[0].State != StateReservedUnpaid || !got[0].From.Equal(from) {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestDurationQty(t *testing.T) {
	start := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		d    time.Duration
		unit string
		want int32
	}{
		{48 * time.Hour, "day", 2},
		{49 * time.Hour, "day", 3}, // a started day is a whole day
		{90 * time.Minute, "hour", 2},
		{31 * 24 * time.Hour, "month", 2}, // month is 30 days, fixed
	}
	for _, c := range cases {
		got, err := durationQty(start, start.Add(c.d), c.unit)
		if err != nil || got != c.want {
			t.Errorf("%v %s = %d, %v; want %d", c.d, c.unit, got, err, c.want)
		}
	}
	two := int32(2)
	if err := checkDuration(1, &two, nil, "day"); err == nil {
		t.Error("1 day under a 2-day minimum was accepted")
	}
	if err := checkDuration(100, nil, nil, "day"); err != nil {
		t.Errorf("no bounds must mean no limit, got %v", err)
	}
}

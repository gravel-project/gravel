package seedhours_test

import (
	"testing"
	"time"

	"github.com/gravel-project/gravel/discord/seedhours"
)

func TestLast(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	h, probs := seedhours.Parse([]seedhours.Window{
		{Days: []string{"wed"}, From: "18:00", To: "22:00"},
		{Days: []string{"fri"}, From: "22:00", To: "02:00"}, // overnight into Saturday
	})
	if len(probs) != 0 {
		t.Fatal(probs)
	}
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.ParseInLocation("2006-01-02 15:04", s, chicago)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		now, start, end string
	}{
		{"2026-10-14 22:00", "2026-10-14 18:00", "2026-10-14 22:00"}, // the moment it ends
		{"2026-10-14 21:59", "2026-10-09 22:00", "2026-10-10 02:00"}, // still open: the last ended is Friday's
		{"2026-10-15 09:00", "2026-10-14 18:00", "2026-10-14 22:00"},
		{"2026-10-17 01:00", "2026-10-14 18:00", "2026-10-14 22:00"}, // Friday's overnight still open
		{"2026-10-17 02:00", "2026-10-16 22:00", "2026-10-17 02:00"}, // it ends on Saturday
		{"2026-10-20 12:00", "2026-10-16 22:00", "2026-10-17 02:00"},
		{"2026-11-04 22:30", "2026-11-04 18:00", "2026-11-04 22:00"}, // after the change to CST: still 18:00 local
	}
	for _, c := range cases {
		start, end, ok := h.Last(at(c.now), chicago)
		if !ok || !start.Equal(at(c.start)) || !end.Equal(at(c.end)) {
			t.Errorf("Last(%s) = %s – %s %t, want %s – %s", c.now, start.In(chicago), end.In(chicago), ok, c.start, c.end)
		}
	}
	if _, _, ok := (seedhours.Hours{}).Last(time.Now(), chicago); ok {
		t.Error("no window, no last")
	}
}
